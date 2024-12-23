package safemap

import "sync"

// SafeMap is a generic struct to manage thread-safe operations on a map
type SafeMap[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]V
}

// New creates and initializes a new SafeMap
func New[K comparable, V any]() *SafeMap[K, V] {
	return &SafeMap[K, V]{
		m: make(map[K]V),
	}
}

// SafeWrite safely writes a key-value pair to the map
func (sm *SafeMap[K, V]) Write(key K, value V) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.m[key] = value
}

// SafeRead safely reads a value for a given key from the map
// Returns the value and a boolean indicating if the key exists
func (sm *SafeMap[K, V]) Read(key K) (V, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	value, ok := sm.m[key]
	return value, ok
}

// SafeDelete safely deletes a key from the map
func (sm *SafeMap[K, V]) Delete(key K) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.m, key)
}

// Iterate iterates over the map and calls the given function for each key-value pair
func (sm *SafeMap[K, V]) Iterate(fn func(key K, value V)) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for key, value := range sm.m {
		fn(key, value)
	}
}
