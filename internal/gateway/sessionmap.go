package gateway

import "sync"

// sessionKeyMap is a tiny concurrent map[string]string used by
// stickySessionPolicy to remember "session abc → key id default:1234".
// Bounded by the number of concurrent live sessions; no eviction
// needed for the in-process lifetime of normal mesh runs.
type sessionKeyMap struct {
	mu sync.RWMutex
	m  map[string]string
}

func (s *sessionKeyMap) get(session string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.m == nil {
		return "", false
	}
	v, ok := s.m[session]
	return v, ok
}

func (s *sessionKeyMap) set(session, keyID string) {
	s.mu.Lock()
	if s.m == nil {
		s.m = make(map[string]string)
	}
	s.m[session] = keyID
	s.mu.Unlock()
}
