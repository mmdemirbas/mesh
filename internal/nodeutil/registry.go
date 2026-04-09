// Package nodeutil provides shared utilities for node lifecycle management.
package nodeutil

import "sync"

// Registry is a thread-safe list of active nodes. Used by clipsync and filesync
// to track running nodes for admin API access.
type Registry[T any] struct {
	mu    sync.RWMutex
	nodes []*T
}

// Register adds a node to the registry.
func (r *Registry[T]) Register(n *T) {
	r.mu.Lock()
	r.nodes = append(r.nodes, n)
	r.mu.Unlock()
}

// Unregister removes a node from the registry.
func (r *Registry[T]) Unregister(n *T) {
	r.mu.Lock()
	for i, node := range r.nodes {
		if node == n {
			r.nodes = append(r.nodes[:i], r.nodes[i+1:]...)
			break
		}
	}
	r.mu.Unlock()
}

// ForEach calls fn with the read lock held for each registered node.
func (r *Registry[T]) ForEach(fn func(*T)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, n := range r.nodes {
		fn(n)
	}
}
