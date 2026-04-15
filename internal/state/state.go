package state

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// evictionInterval controls how often the background goroutine sweeps for stale entries.
	evictionInterval = 5 * time.Minute

	// componentTTL is how long a component entry survives without an Update call.
	// Active components refresh on every state change; stale orphans get cleaned up.
	componentTTL = 1 * time.Hour
)

type Status string

const (
	Starting   Status = "starting"
	Listening  Status = "listening"
	Connecting Status = "connecting"
	Connected  Status = "connected"
	Failed     Status = "failed"
	Retrying   Status = "retrying"
)

// Metrics tracks live connection activity using lock-free atomic counters.
// A single Metrics instance is shared across all forwards within a forward set
// so the dashboard can show aggregate activity per connection.
type Metrics struct {
	BytesTx   atomic.Int64
	BytesRx   atomic.Int64
	Streams   atomic.Int32
	TokensIn  atomic.Int64 // gateway-only: prompt tokens accumulated across requests
	TokensOut atomic.Int64 // gateway-only: completion tokens accumulated across requests
	StartTime atomic.Int64 // unix nanoseconds; reset on each reconnect
}

// Reset zeroes counters and sets StartTime to now. Used on reconnect.
func (m *Metrics) Reset() {
	m.BytesTx.Store(0)
	m.BytesRx.Store(0)
	m.Streams.Store(0)
	m.TokensIn.Store(0)
	m.TokensOut.Store(0)
	m.StartTime.Store(time.Now().UnixNano())
}

type Component struct {
	Type        string    `json:"type"`                 // "proxy", "relay", "server", "connection"
	ID          string    `json:"id"`                   // unique identifier
	Status      Status    `json:"status"`               // current status
	Message     string    `json:"message"`              // error or target info
	BoundAddr   string    `json:"bound_addr"`           // active resolved listener address
	PeerAddr    string    `json:"peer_addr"`            // resolved remote peer address (connections)
	FileCount   int       `json:"file_count,omitempty"` // tracked file count (filesync folders)
	LastSync    time.Time `json:"last_sync,omitempty"`  // last successful sync time (filesync)
	LastUpdated time.Time `json:"last_updated"`         // used by TTL eviction
}

type State struct {
	mu         sync.RWMutex
	components map[string]Component
	metrics    sync.Map // key -> *Metrics
}

var Global = &State{
	components: make(map[string]Component),
}

func (s *State) Update(compType, id string, status Status, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := compType + ":" + id
	comp := s.components[key] // retrieve existing to preserve BoundAddr
	comp.Type = compType
	comp.ID = id
	comp.Status = status
	comp.Message = msg
	comp.LastUpdated = time.Now()
	s.components[key] = comp
}

func (s *State) UpdateBind(compType, id, boundAddr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := compType + ":" + id
	comp := s.components[key]
	comp.BoundAddr = boundAddr
	comp.LastUpdated = time.Now()
	s.components[key] = comp
}

func (s *State) UpdatePeer(compType, id, peerAddr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := compType + ":" + id
	comp := s.components[key]
	comp.PeerAddr = peerAddr
	comp.LastUpdated = time.Now()
	s.components[key] = comp
}

func (s *State) UpdateFileCount(compType, id string, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := compType + ":" + id
	comp := s.components[key]
	comp.FileCount = count
	comp.LastUpdated = time.Now()
	s.components[key] = comp
}

func (s *State) UpdateLastSync(compType, id string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := compType + ":" + id
	comp := s.components[key]
	comp.LastSync = t
	comp.LastUpdated = time.Now()
	s.components[key] = comp
}

// GetMetrics returns the Metrics for a component, creating one if needed.
func (s *State) GetMetrics(compType, id string) *Metrics {
	key := compType + ":" + id
	if v, ok := s.metrics.Load(key); ok {
		return v.(*Metrics)
	}
	m := &Metrics{}
	actual, _ := s.metrics.LoadOrStore(key, m)
	return actual.(*Metrics)
}

// SnapshotMetrics returns a point-in-time copy of all metrics keyed by component key.
func (s *State) SnapshotMetrics() map[string]*Metrics {
	m := make(map[string]*Metrics)
	s.metrics.Range(func(key, value any) bool {
		m[key.(string)] = value.(*Metrics)
		return true
	})
	return m
}

// FullSnapshot holds a consistent view of components and their metrics.
type FullSnapshot struct {
	Components map[string]Component
	Metrics    map[string]*Metrics
}

// SnapshotFull returns components and metrics together. Components are read
// under mu.RLock; metrics use a separate sync.Map, so the two snapshots are
// NOT strictly atomic — a brief divergence is possible. This is acceptable
// for display and Prometheus export. Callers that need additional data from
// other packages (e.g. auth failures from tunnel) should snapshot those
// separately immediately after.
func (s *State) SnapshotFull() FullSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	comps := make(map[string]Component, len(s.components))
	for k, v := range s.components {
		comps[k] = v
	}
	metrics := make(map[string]*Metrics)
	s.metrics.Range(func(key, value any) bool {
		metrics[key.(string)] = value.(*Metrics)
		return true
	})
	return FullSnapshot{Components: comps, Metrics: metrics}
}

func (s *State) Delete(compType, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.components, compType+":"+id)
}

func (s *State) DeleteMetrics(compType, id string) {
	s.metrics.Delete(compType + ":" + id)
}

// Sizes returns the number of tracked components and metrics entries without
// allocating snapshot copies.
func (s *State) Sizes() (components, metrics int) {
	s.mu.RLock()
	components = len(s.components)
	s.mu.RUnlock()
	s.metrics.Range(func(_, _ any) bool { metrics++; return true })
	return components, metrics
}

func (s *State) Snapshot() map[string]Component {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := make(map[string]Component)
	for k, v := range s.components {
		m[k] = v
	}
	return m
}

// StartEviction runs a background goroutine that periodically removes stale
// component and metric entries. It stops when ctx is cancelled.
func (s *State) StartEviction(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(evictionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.evictStale(time.Now())
			}
		}
	}()
}

// evictStale removes components not updated within componentTTL and their orphaned metrics.
// Components in stable states (Listening, Connected) are exempt — they represent
// long-running goroutines that update state once at startup and then serve
// indefinitely. Only transient states (Starting, Connecting, Retrying, Failed)
// are subject to eviction, since those indicate a component that likely crashed
// or leaked without proper cleanup.
func (s *State) evictStale(now time.Time) {
	cutoff := now.Add(-componentTTL)

	s.mu.Lock()
	var evicted []string
	for key, comp := range s.components {
		if comp.Status == Listening || comp.Status == Connected {
			continue // stable long-lived components are never evicted
		}
		if !comp.LastUpdated.IsZero() && comp.LastUpdated.Before(cutoff) {
			delete(s.components, key)
			evicted = append(evicted, key)
		}
	}
	s.mu.Unlock()

	for _, key := range evicted {
		s.metrics.Delete(key)
	}

	if len(evicted) > 0 {
		slog.Debug("evicted stale state entries", "count", len(evicted))
	}
}
