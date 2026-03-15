package state

import (
	"sync"
	"testing"
)

func newState() *State {
	return &State{components: make(map[string]Component)}
}

func TestUpdate(t *testing.T) {
	s := newState()
	s.Update("proxy", "127.0.0.1:1080", Listening, "ready")

	snap := s.Snapshot()
	comp, ok := snap["proxy:127.0.0.1:1080"]
	if !ok {
		t.Fatal("component not found after Update")
	}
	if comp.Type != "proxy" {
		t.Errorf("Type = %q, want %q", comp.Type, "proxy")
	}
	if comp.ID != "127.0.0.1:1080" {
		t.Errorf("ID = %q, want %q", comp.ID, "127.0.0.1:1080")
	}
	if comp.Status != Listening {
		t.Errorf("Status = %q, want %q", comp.Status, Listening)
	}
	if comp.Message != "ready" {
		t.Errorf("Message = %q, want %q", comp.Message, "ready")
	}
}

func TestUpdatePreservesBoundAddr(t *testing.T) {
	s := newState()
	s.Update("proxy", "id1", Starting, "")
	s.UpdateBind("proxy", "id1", "127.0.0.1:9999")
	s.Update("proxy", "id1", Listening, "ok")

	snap := s.Snapshot()
	comp := snap["proxy:id1"]
	if comp.BoundAddr != "127.0.0.1:9999" {
		t.Errorf("BoundAddr = %q, want %q (should be preserved across Update)", comp.BoundAddr, "127.0.0.1:9999")
	}
	if comp.Status != Listening {
		t.Errorf("Status = %q, want %q", comp.Status, Listening)
	}
}

func TestUpdateBind(t *testing.T) {
	s := newState()
	s.Update("relay", "r1", Starting, "")
	s.UpdateBind("relay", "r1", "0.0.0.0:8080")

	snap := s.Snapshot()
	if snap["relay:r1"].BoundAddr != "0.0.0.0:8080" {
		t.Errorf("BoundAddr = %q, want %q", snap["relay:r1"].BoundAddr, "0.0.0.0:8080")
	}
}

func TestDelete(t *testing.T) {
	s := newState()
	s.Update("server", "s1", Listening, "")
	s.Delete("server", "s1")

	snap := s.Snapshot()
	if _, ok := snap["server:s1"]; ok {
		t.Error("component still present after Delete")
	}
}

func TestDeleteNonExistent(t *testing.T) {
	s := newState()
	s.Delete("server", "nonexistent") // should not panic
}

func TestSnapshotIsACopy(t *testing.T) {
	s := newState()
	s.Update("proxy", "p1", Listening, "")

	snap := s.Snapshot()
	snap["proxy:p1"] = Component{Message: "mutated"}

	snap2 := s.Snapshot()
	if snap2["proxy:p1"].Message == "mutated" {
		t.Error("Snapshot returned a reference, not a copy")
	}
}

func TestSnapshotEmpty(t *testing.T) {
	s := newState()
	snap := s.Snapshot()
	if len(snap) != 0 {
		t.Errorf("Snapshot of empty state has %d entries", len(snap))
	}
}

func TestStatusConstants(t *testing.T) {
	statuses := map[Status]string{
		Starting:   "starting",
		Listening:  "listening",
		Connecting: "connecting",
		Connected:  "connected",
		Failed:     "failed",
		Retrying:   "retrying",
	}
	for s, want := range statuses {
		if string(s) != want {
			t.Errorf("Status %v = %q, want %q", s, string(s), want)
		}
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := newState()
	var wg sync.WaitGroup

	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i%26))
			s.Update("proxy", id, Listening, "")
			s.UpdateBind("proxy", id, "127.0.0.1:8080")
			s.Snapshot()
			s.Delete("proxy", id)
		}(i)
	}
	wg.Wait()
}
