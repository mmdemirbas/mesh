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
	Type    string `json:"type"`    // "proxy", "relay", "server", "connection"
	ID      string `json:"id"`      // unique identifier
	Status  Status `json:"status"`  // current status
	Message string `json:"message"` // error or target info
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
	s.components[compType+":"+id] = Component{
		Type:    compType,
		ID:      id,
		Status:  status,
		Message: msg,
	}
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
