package state

import (
	"sync"
	"sync/atomic"
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
	StartTime atomic.Int64 // unix nanoseconds; reset on each reconnect
}

type Component struct {
	Type      string `json:"type"`       // "proxy", "relay", "server", "connection"
	ID        string `json:"id"`         // unique identifier
	Status    Status `json:"status"`     // current status
	Message   string `json:"message"`    // error or target info
	BoundAddr string `json:"bound_addr"` // active resolved listener address
	PeerAddr  string `json:"peer_addr"`  // resolved remote peer address (connections)
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
	s.components[key] = comp
}

func (s *State) UpdateBind(compType, id, boundAddr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := compType + ":" + id
	comp := s.components[key]
	comp.BoundAddr = boundAddr
	s.components[key] = comp
}

func (s *State) UpdatePeer(compType, id, peerAddr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := compType + ":" + id
	comp := s.components[key]
	comp.PeerAddr = peerAddr
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

func (s *State) Delete(compType, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.components, compType+":"+id)
}

func (s *State) DeleteMetrics(compType, id string) {
	s.metrics.Delete(compType + ":" + id)
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
