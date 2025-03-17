// Package liteLRU implements a high-performance LRU (Least Recently Used) cache
// designed for HTTP routing. It provides thread-safe operations, efficient memory
// management through object pooling, and optimized string handling.
package liteLRU

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Param represents a key-value parameter in a cache entry.
// Typically used for route parameters in HTTP request handlers.
type Param struct {
	Key   string // Parameter name
	Value string // Parameter value
}

// HandlerFunc represents a handler function to be executed when a cached route is matched.
// In a routing context, this would be the function called when a route is accessed.
type HandlerFunc func()

// routeCacheKey uniquely identifies an entry in the LRU cache.
// It combines HTTP method and path to form a composite key.
type routeCacheKey struct {
	method string // HTTP method (GET, POST, etc.)
	path   string // Request path
}

// entry represents a single item in the LRU cache.
// It contains the cached data and pointers for the doubly-linked list.
type entry struct {
	key     routeCacheKey // The cache key (method + path)
	handler HandlerFunc   // The handler function for this route
	params  []Param       // Route parameters
	prev    int           // Index of the previous entry in the doubly-linked list
	next    int           // Index of the next entry in the doubly-linked list
}

// LRUCache implements a thread-safe least recently used cache with fixed capacity.
// It uses an array-based doubly-linked list for O(1) LRU operations and maintains
// hit/miss statistics for performance monitoring.
type LRUCache struct {
	capacity  int                   // Maximum number of entries the cache can hold
	mutex     sync.RWMutex          // Read-write mutex for thread safety
	entries   []entry               // Array of cache entries
	indices   map[routeCacheKey]int // Map from key to index in entries
	head      int                   // Index of the most recently used entry
	tail      int                   // Index of the least recently used entry
	hits      int64                 // Number of cache hits (atomic counter)
	misses    int64                 // Number of cache misses (atomic counter)
	maxParams int                   // Configurable max parameters per entry
}

// nextPowerOfTwo rounds up to the next power of two.
// This improves performance by aligning with hash table implementation details.
// For example: 10 becomes 16, 120 becomes 128, etc.
func nextPowerOfTwo(n int) int {
	if n <= 0 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n++
	return n
}

// Define multiple sync.Pools for different parameter slice sizes.
// This reduces GC pressure by reusing parameter slices based on their capacity.
var paramSlicePools = [5]sync.Pool{
	{New: func() interface{} { return make([]Param, 0, 4) }},  // Capacity 4
	{New: func() interface{} { return make([]Param, 0, 8) }},  // Capacity 8
	{New: func() interface{} { return make([]Param, 0, 16) }}, // Capacity 16
	{New: func() interface{} { return make([]Param, 0, 32) }}, // Capacity 32
	{New: func() interface{} { return make([]Param, 0, 64) }}, // Capacity 64
}

// getParamSlice retrieves a parameter slice from the appropriate pool based on paramCount.
// This function optimizes memory usage by selecting a pool with an appropriate capacity
// for the requested number of parameters.
func getParamSlice(paramCount int) []Param {
	if paramCount <= 4 {
		return paramSlicePools[0].Get().([]Param)[:0]
	} else if paramCount <= 8 {
		return paramSlicePools[1].Get().([]Param)[:0]
	} else if paramCount <= 16 {
		return paramSlicePools[2].Get().([]Param)[:0]
	} else if paramCount <= 32 {
		return paramSlicePools[3].Get().([]Param)[:0]
	} else {
		return paramSlicePools[4].Get().([]Param)[:0]
	}
}

// putParamSlice returns a parameter slice to the appropriate pool based on its capacity.
// This function recycles parameter slices to reduce garbage collection overhead.
// Slices with capacities that don't match a pool are left for the garbage collector.
func putParamSlice(s []Param) {
	cap := cap(s)
	if cap == 4 {
		paramSlicePools[0].Put(s)
	} else if cap == 8 {
		paramSlicePools[1].Put(s)
	} else if cap == 16 {
		paramSlicePools[2].Put(s)
	} else if cap == 32 {
		paramSlicePools[3].Put(s)
	} else if cap == 64 {
		paramSlicePools[4].Put(s)
	}
	// Slices with unexpected capacities are discarded (handled by GC)
}

// Simple string interning for method and path.
// This reduces memory usage by storing only one copy of each unique string.
var stringInterner = struct {
	sync.RWMutex
	m map[string]string
}{
	m: make(map[string]string, 16), // Preallocate for common HTTP methods
}

// internString returns a single canonical instance of the given string.
// If the string has been seen before, the stored version is returned.
// Otherwise, the input string becomes the canonical version.
// This reduces memory usage when the same strings are frequently used.
func internString(s string) string {
	stringInterner.RLock()
	if interned, ok := stringInterner.m[s]; ok {
		stringInterner.RUnlock()
		return interned
	}
	stringInterner.RUnlock()
	stringInterner.Lock()
	defer stringInterner.Unlock()
	if interned, ok := stringInterner.m[s]; ok {
		return interned
	}
	stringInterner.m[s] = s // Store the string itself as the canonical copy
	return s
}

// NewLRUCache creates a new LRU cache with the specified capacity and maxParams.
// The capacity determines how many entries can be stored before eviction begins.
// The maxParams parameter configures the maximum number of parameters per entry.
// The function applies reasonable defaults and bounds if invalid values are provided.
func NewLRUCache(capacity, maxParams int) *LRUCache {
	// Set defaults if invalid values provided
	if capacity <= 0 {
		capacity = 1024 // Default size
	}

	// Set a reasonable upper limit to prevent unexpected issues
	if capacity > 16384 {
		capacity = 16384
	}

	// Round capacity to next power of two for better performance
	capacity = nextPowerOfTwo(capacity)

	if maxParams <= 0 {
		maxParams = 10 // Default max parameters
	}

	c := &LRUCache{
		capacity:  capacity,
		maxParams: maxParams,
		entries:   make([]entry, capacity),
		indices:   make(map[routeCacheKey]int, capacity*2), // Oversize to avoid rehashing
		head:      0,
		tail:      capacity - 1,
	}

	// Initialize the circular doubly-linked list
	for i := 0; i < capacity; i++ {
		c.entries[i].next = (i + 1) % capacity
		c.entries[i].prev = (i - 1 + capacity) % capacity
	}

	return c
}

// Add adds a new entry to the cache or updates an existing one.
// If the key already exists, the entry is updated and moved to the front of the LRU list.
// If the key doesn't exist, the least recently used entry is replaced with the new entry.
// This method is thread-safe and optimizes memory usage through string interning and slice pooling.
func (c *LRUCache) Add(method, path string, handler HandlerFunc, params []Param) {
	// Intern strings to reduce allocations
	method = internString(method)
	path = internString(path)
	c.mutex.Lock()
	defer c.mutex.Unlock()
	key := routeCacheKey{method: method, path: path}

	// Check if the key already exists
	if idx, exists := c.indices[key]; exists {
		entry := &c.entries[idx]
		entry.handler = handler

		// Reuse params slice if capacity is sufficient
		if cap(entry.params) >= len(params) {
			entry.params = entry.params[:len(params)]
			copy(entry.params, params)
		} else {
			if entry.params != nil {
				putParamSlice(entry.params)
			}

			newParams := getParamSlice(len(params))
			copy(newParams, params)
			entry.params = newParams
		}

		c.moveToFront(idx)
		return
	}

	// New entry: reuse the tail slot
	idx := c.tail
	entry := &c.entries[idx]
	oldKey := entry.key

	if oldKey.method != "" || oldKey.path != "" {
		delete(c.indices, oldKey)
	}

	if entry.params != nil {
		putParamSlice(entry.params)
	}

	// Update the existing key struct instead of creating a new one
	entry.key.method = method
	entry.key.path = path
	entry.handler = handler

	// Allocate and copy params
	newParams := getParamSlice(len(params))
	copy(newParams, params)
	entry.params = newParams

	c.indices[entry.key] = idx
	c.moveToFront(idx)
}

// Get retrieves an entry from the cache.
// It returns the handler function, parameters, and a boolean indicating whether the entry was found.
// If the entry is found, it's moved to the front of the LRU list.
// This method is thread-safe and includes panic recovery for robustness.
func (c *LRUCache) Get(method, path string) (HandlerFunc, []Param, bool) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("Recovered from panic in Get: %v\n", r)
			return
		}
	}()

	// Intern strings for consistency
	method = internString(method)
	path = internString(path)
	key := routeCacheKey{method: method, path: path}

	c.mutex.RLock()
	idx, exists := c.indices[key]
	if !exists {
		atomic.AddInt64(&c.misses, 1)
		c.mutex.RUnlock()
		return nil, nil, false
	}

	entry := &c.entries[idx]
	handler := entry.handler

	var params []Param
	if len(entry.params) > 0 {
		params = getParamSlice(len(entry.params))
		copy(params, entry.params)
	} else {
		params = nil
	}

	c.mutex.RUnlock()

	c.mutex.Lock()
	c.moveToFront(idx)
	c.mutex.Unlock()

	atomic.AddInt64(&c.hits, 1)
	return handler, params, true
}

// moveToFront moves an entry to the front of the list (most recently used).
// This maintains the LRU ordering of the cache entries.
// The method includes bounds checking and panic recovery for robustness.
func (c *LRUCache) moveToFront(idx int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("Recovered from panic in moveToFront: %v\n", r)
		}
	}()

	// Already at front, nothing to do
	if idx == c.head {
		return
	}

	// Safety check for invalid index
	if idx < 0 || idx >= c.capacity {
		fmt.Printf("Warning: Attempted to move invalid index %d in LRU cache with capacity %d\n", idx, c.capacity)
		return
	}

	// Remove from current position
	entry := &c.entries[idx]
	prevIdx := entry.prev
	nextIdx := entry.next
	c.entries[prevIdx].next = nextIdx
	c.entries[nextIdx].prev = prevIdx

	// Update tail if we moved the tail
	if idx == c.tail {
		c.tail = prevIdx
	}

	// Insert at front
	oldHead := c.head
	oldHeadPrev := c.entries[oldHead].prev

	entry.next = oldHead
	entry.prev = oldHeadPrev
	c.entries[oldHead].prev = idx
	c.entries[oldHeadPrev].next = idx

	// Update head
	c.head = idx
}

// Clear removes all entries from the cache and returns param slices to pools.
// It resets the cache to its initial state while properly cleaning up resources.
// This method is thread-safe and includes panic recovery for robustness.
func (c *LRUCache) Clear() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("Recovered from panic in Clear: %v\n", r)
		}
	}()

	c.mutex.Lock()
	defer c.mutex.Unlock()

	for i := range c.entries {
		if c.entries[i].params != nil {
			putParamSlice(c.entries[i].params)
			c.entries[i].params = nil
		}

		c.entries[i].key = routeCacheKey{}
		c.entries[i].handler = nil
	}

	c.indices = make(map[routeCacheKey]int, c.capacity*2)
	c.head = 0
	c.tail = c.capacity - 1

	// Re-initialize the linked list
	for i := 0; i < c.capacity; i++ {
		c.entries[i].next = (i + 1) % c.capacity
		c.entries[i].prev = (i - 1 + c.capacity) % c.capacity
	}

	atomic.StoreInt64(&c.hits, 0)
	atomic.StoreInt64(&c.misses, 0)
}

// Stats returns cache hit/miss statistics.
// It provides the number of cache hits, misses, and the hit ratio.
// These values are useful for monitoring and tuning cache performance.
func (c *LRUCache) Stats() (hits, misses int64, ratio float64) {
	hits = atomic.LoadInt64(&c.hits)
	misses = atomic.LoadInt64(&c.misses)
	total := hits + misses
	if total > 0 {
		ratio = float64(hits) / float64(total)
	}

	return
}
