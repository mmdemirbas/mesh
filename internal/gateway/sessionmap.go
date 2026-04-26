package gateway

import (
	"container/list"
	"sync"
)

// maxStickySessionEntries caps the number of session→key bindings a
// single stickySessionPolicy will hold. The previous unbounded map
// grew with the cardinality of session ids the gateway saw — fine for
// stable workloads with a few hundred concurrent sessions, but a
// growing concern when an upstream client (a benchmarking tool, a
// fuzzing harness, or a misconfigured agent loop) emits unbounded
// distinct session ids. Capping at 10k keeps the working set tiny
// while still covering any realistic interactive workload. Sessions
// evicted because they fell out of LRU window simply get a fresh
// binding on their next request — no functional regression, just a
// transient cache miss.
const maxStickySessionEntries = 10_000

// sessionKeyMap is a small concurrent LRU keyed by session id with a
// key id value. Used by stickySessionPolicy to remember
// "session abc → key id default:1234" for the lifetime of a session.
// The cap protects against unbounded growth when callers churn
// session ids; eviction is least-recently-used (touched on get and
// set). Zero value is ready to use.
type sessionKeyMap struct {
	mu sync.Mutex // guards both m and order; the LRU bump on get
	// mutates state, so RWMutex would not help.
	m     map[string]*list.Element
	order *list.List // front = most recently used, back = next to evict
}

// sessionEntry pairs the key with its session id so eviction from the
// back of the list can clean up the map without an extra lookup.
type sessionEntry struct {
	session string
	keyID   string
}

func (s *sessionKeyMap) get(session string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		return "", false
	}
	el, ok := s.m[session]
	if !ok {
		return "", false
	}
	s.order.MoveToFront(el)
	return el.Value.(*sessionEntry).keyID, true
}

func (s *sessionKeyMap) set(session, keyID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]*list.Element)
		s.order = list.New()
	}
	if el, ok := s.m[session]; ok {
		el.Value.(*sessionEntry).keyID = keyID
		s.order.MoveToFront(el)
		return
	}
	el := s.order.PushFront(&sessionEntry{session: session, keyID: keyID})
	s.m[session] = el
	for s.order.Len() > maxStickySessionEntries {
		victim := s.order.Back()
		if victim == nil {
			break
		}
		s.order.Remove(victim)
		delete(s.m, victim.Value.(*sessionEntry).session)
	}
}

// len reports the current entry count. Test-only convenience.
func (s *sessionKeyMap) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.order == nil {
		return 0
	}
	return s.order.Len()
}
