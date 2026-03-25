package state

import (
	"sync"
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

func (s *State) Delete(compType, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.components, compType+":"+id)
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
