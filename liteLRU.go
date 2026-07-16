// Package liteLRU implements a high-performance LRU (Least Recently Used) cache
// designed for HTTP routing. It provides thread-safe operations, efficient memory
// management through object pooling, and a fully lock-free architecture.
package liteLRU

import (
	"fmt"
	"math/bits"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/xDarkicex/memory"
)

// Param represents a key-value parameter in a cache entry.
type Param struct {
	Key   string
	Value string
}

// HandlerFunc represents a handler function to be executed when a cached route is matched.
type HandlerFunc func()

// hashRoute mixes the method and path into a fast 64-bit non-cryptographic hash (FNV-1a variant).
func hashRoute(method, path string) uint64 {
	hash := uint64(14695981039346656037)
	for i := 0; i < len(method); i++ {
		hash ^= uint64(method[i])
		hash *= 1099511628211
	}
	hash ^= uint64('/')
	hash *= 1099511628211
	for i := 0; i < len(path); i++ {
		hash ^= uint64(path[i])
		hash *= 1099511628211
	}
	return hash
}

// chunk represents a group of 64 slots with bitmasks for O(1) eviction logic.
// Padded to 64 bytes to prevent false sharing between CPU cache lines.
type chunk struct {
	valid    atomic.Uint64
	accessed atomic.Uint64
	writing  atomic.Uint64
	_        [40]byte
}

// slotState holds the seqlock, padded to a full 64-byte cache line 
// to completely eliminate false-sharing during concurrent writes.
type slotState struct {
	seq atomic.Uint32
	_   [60]byte
}

// statStripe shards cache statistics across 64 independent cache lines
// to prevent global atomic contention during high-throughput parallel access.
type statStripe struct {
	hits   atomic.Int64
	misses atomic.Int64
	_      [48]byte
}

//go:nosplit
func noescape(p unsafe.Pointer) unsafe.Pointer {
	x := uintptr(p)
	return unsafe.Pointer(x ^ 0)
}

type atomicString struct {
	ptr atomic.Pointer[byte]
	len atomic.Int64
}

func (a *atomicString) Store(s string) {
	a.ptr.Store(unsafe.StringData(s))
	a.len.Store(int64(len(s)))
}

func (a *atomicString) Load() string {
	ptr := a.ptr.Load()
	l := a.len.Load()
	if ptr == nil {
		return ""
	}
	return unsafe.String(ptr, int(l))
}

type atomicSlice struct {
	ptr atomic.Pointer[Param]
	len atomic.Int64
	cap atomic.Int64
}

func (a *atomicSlice) Store(s []Param) {
	a.ptr.Store(unsafe.SliceData(s))
	a.len.Store(int64(len(s)))
	a.cap.Store(int64(cap(s)))
}

func (a *atomicSlice) Load() []Param {
	ptr := a.ptr.Load()
	if ptr == nil {
		return nil
	}
	l := a.len.Load()
	c := a.cap.Load()
	s := unsafe.Slice(ptr, c)
	return s[:l]
}

type atomicHandler struct {
	ptr atomic.Pointer[byte]
}

func (a *atomicHandler) Store(h HandlerFunc) {
	if h == nil {
		a.ptr.Store(nil)
		return
	}
	p := *(*unsafe.Pointer)(noescape(unsafe.Pointer(&h)))
	a.ptr.Store((*byte)(p))
}

func (a *atomicHandler) Load() HandlerFunc {
	ptr := a.ptr.Load()
	if ptr == nil {
		return nil
	}
	return *(*HandlerFunc)(unsafe.Pointer(&ptr))
}

// LRUCache implements an ultra-low latency lock-free cache with fixed capacity.
// It uses a Chunked Bitmask CLOCK algorithm for O(1) eviction.
type LRUCache struct {
	capacity  uint32
	maxParams int

	// Lock-free HashMap maps a route hash to a pointer to an SoA index
	indexMap *memory.HashMap

	// Pre-allocated index pointers to prevent checkptr panics
	indices []uint32

	// Structure of Arrays (SoA) for fast contiguous memory access
	methods  []atomicString
	paths    []atomicString
	handlers []atomicHandler
	params   []atomicSlice

	// Concurrency control
	states []slotState // padded seqlocks to prevent read-tearing
	chunks []chunk     // padded bitmasks (64 slots per chunk)

	numGroups  uint32
	stats      [64]statStripe
}

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

var paramSlicePools = [5]sync.Pool{
	{New: func() interface{} { return make([]Param, 0, 4) }},
	{New: func() interface{} { return make([]Param, 0, 8) }},
	{New: func() interface{} { return make([]Param, 0, 16) }},
	{New: func() interface{} { return make([]Param, 0, 32) }},
	{New: func() interface{} { return make([]Param, 0, 64) }},
}

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
}

// NewLRUCache creates a new fully lock-free LRU cache.
func NewLRUCache(capacity, maxParams int) *LRUCache {
	if capacity <= 0 {
		capacity = 1024
	}
	capacity = nextPowerOfTwo(capacity)
	if capacity < 64 {
		capacity = 64
	}

	if maxParams <= 0 {
		maxParams = 10
	}

	hm, err := memory.NewHashMap(memory.HashMapConfig{
		Capacity: uint64(capacity * 2), // oversized to minimize collisions
	})
	if err != nil {
		panic(fmt.Sprintf("liteLRU: failed to initialize off-heap memory map: %v", err))
	}

	numGroups := uint32(capacity / 64)

	c := &LRUCache{
		capacity:  uint32(capacity),
		maxParams: maxParams,
		indexMap:  hm,
		methods:   make([]atomicString, capacity),
		paths:     make([]atomicString, capacity),
		handlers:  make([]atomicHandler, capacity),
		params:    make([]atomicSlice, capacity),
		states:    make([]slotState, capacity),
		chunks:    make([]chunk, numGroups),
		numGroups: uint32(numGroups),
		indices:   make([]uint32, capacity),
	}

	for i := uint32(0); i < uint32(capacity); i++ {
		c.indices[i] = i
	}

	return c
}

// findVictim uses bitwise operations to instantly find an eviction victim in O(1) time.
// It uses the hash to pick a starting group, naturally distributing the eviction sweep 
// and eliminating the need for a highly-contended global clock hand.
func (c *LRUCache) findVictim(hash uint64) uint32 {
	startGroup := uint32(hash % uint64(c.numGroups))
	for offset := uint32(0); ; offset++ {
		group := (startGroup + offset) % c.numGroups
		chk := &c.chunks[group]

		for {
			validBits := chk.valid.Load()
			accessedBits := chk.accessed.Load()
			writingBits := chk.writing.Load()

			// Candidates: not valid (empty), or valid but not accessed
			candidates := ^validBits | (validBits & ^accessedBits)
			// Exclude currently writing slots
			candidates &= ^writingBits

			if candidates != 0 {
				bit := uint32(bits.TrailingZeros64(candidates))

				// Attempt to claim this bit for writing
				if chk.writing.CompareAndSwap(writingBits, writingBits|(1<<bit)) {
					return group*64 + bit
				}
				continue // CAS failed, retry this chunk
			}

			// No candidates available in this chunk.
			// Clear the accessed bits of currently valid items to give them a second chance,
			// while preserving any concurrent access bits set by readers.
			chk.accessed.And(^validBits)
			break // move to the next chunk group
		}
	}
}

// Add adds a new entry to the cache or updates an existing one.
func (c *LRUCache) Add(method, path string, handler HandlerFunc, params []Param) {
	hash := hashRoute(method, path)

	// O(1) Bitwise victim selection
	victimIdx := c.findVictim(hash)
	group := victimIdx / 64
	bit := victimIdx % 64
	chk := &c.chunks[group]

	// We own the writing bit. Set seqlock to odd.
	seq := c.states[victimIdx].seq.Load()
	c.states[victimIdx].seq.Store(seq + 1) // odd

	// If it was valid previously, cleanly delete the old hash from indexMap
	// to allow memory.HashMap to safely recycle the tombstone and prevent resizes!
	validBits := chk.valid.Load()
	if (validBits & (1 << bit)) != 0 {
		oldHash := hashRoute(c.methods[victimIdx].Load(), c.paths[victimIdx].Load())
		if ptr, found := c.indexMap.Get(oldHash); found && ptr != nil && *(*uint32)(ptr) == victimIdx {
			c.indexMap.Delete(oldHash)
		}
	}

	// Write new data safely under seqlock
	c.methods[victimIdx].Store(method)
	c.paths[victimIdx].Store(path)
	c.handlers[victimIdx].Store(handler)

	oldParams := c.params[victimIdx].Load()
	var newParams []Param
	if len(params) > 0 {
		if cap(oldParams) >= len(params) {
			newParams = oldParams[:len(params)]
			copy(newParams, params)
		} else {
			if oldParams != nil {
				putParamSlice(oldParams)
			}
			newParams = getParamSlice(len(params))
			copy(newParams, params)
		}
	} else {
		if oldParams != nil {
			putParamSlice(oldParams)
		}
		newParams = nil
	}
	c.params[victimIdx].Store(newParams)

	// Mark as accessed
	for {
		acc := chk.accessed.Load()
		if chk.accessed.CompareAndSwap(acc, acc|(1<<bit)) {
			break
		}
	}

	// Mark as valid
	for {
		v := chk.valid.Load()
		if chk.valid.CompareAndSwap(v, v|(1<<bit)) {
			break
		}
	}

	// Insert pointer to the pre-allocated index into the lock-free map
	c.indexMap.Put(hash, unsafe.Pointer(&c.indices[victimIdx]))

	// Finish write: seq becomes even
	c.states[victimIdx].seq.Store(seq + 2)

	// Release writing bit
	for {
		w := chk.writing.Load()
		if chk.writing.CompareAndSwap(w, w & ^(1<<bit)) {
			break
		}
	}
}

// Get retrieves an entry from the cache.
// 100% lock-free, zero allocation, utilizing seqlocks and O(1) bitmask lookups.
func (c *LRUCache) Get(method, path string) (HandlerFunc, []Param, bool) {
	hash := hashRoute(method, path)
	stripeIdx := hash & 63

	ptr, found := c.indexMap.Get(hash)
	if !found || ptr == nil {
		c.stats[stripeIdx].misses.Add(1)
		return nil, nil, false
	}

	idx := *(*uint32)(ptr)
	group := idx / 64
	bit := idx % 64
	chk := &c.chunks[group]

	// Verify the slot is valid
	validBits := chk.valid.Load()
	if (validBits & (1 << bit)) == 0 {
		c.stats[stripeIdx].misses.Add(1)
		return nil, nil, false
	}

	// Start read seqlock
	seq1 := c.states[idx].seq.Load()
	if seq1%2 != 0 {
		c.stats[stripeIdx].misses.Add(1)
		return nil, nil, false
	}

	// Validate method/path against concurrent evictions and collisions
	if c.methods[idx].Load() != method || c.paths[idx].Load() != path {
		c.stats[stripeIdx].misses.Add(1)
		return nil, nil, false
	}

	// Safely read data
	handler := c.handlers[idx].Load()
	params := c.params[idx].Load()

	var copiedParams []Param
	if len(params) > 0 {
		copiedParams = getParamSlice(len(params))
		copy(copiedParams, params)
	}

	// Validate read seqlock
	seq2 := c.states[idx].seq.Load()
	if seq1 != seq2 {
		// Slot was modified while we were reading!
		if copiedParams != nil {
			putParamSlice(copiedParams)
		}
		c.stats[stripeIdx].misses.Add(1)
		return nil, nil, false
	}

	// Mark as accessed for CLOCK via CAS loop
	for {
		acc := chk.accessed.Load()
		if (acc & (1 << bit)) != 0 {
			break // already accessed
		}
		if chk.accessed.CompareAndSwap(acc, acc|(1<<bit)) {
			break
		}
	}

	c.stats[stripeIdx].hits.Add(1)
	return handler, copiedParams, true
}

// Clear gracefully removes all entries from the cache lock-free.
func (c *LRUCache) Clear() {
	hm, _ := memory.NewHashMap(memory.HashMapConfig{
		Capacity: uint64(c.capacity * 2),
	})
	c.indexMap = hm

	for group := uint32(0); group < c.numGroups; group++ {
		chk := &c.chunks[group]
		
		for {
			w := chk.writing.Load()
			// Claim all non-writing slots
			toClaim := ^w
			if chk.writing.CompareAndSwap(w, w|toClaim) {
				break
			}
		}

		// Clear valid and accessed
		chk.valid.Store(0)
		chk.accessed.Store(0)

		for bit := uint32(0); bit < 64; bit++ {
			idx := group*64 + bit
			oldParams := c.params[idx].Load()
			if oldParams != nil {
				putParamSlice(oldParams)
				c.params[idx].Store(nil)
			}
			c.methods[idx].Store("")
			c.paths[idx].Store("")
			c.handlers[idx].Store(nil)
			c.states[idx].seq.Store(0)
		}

		chk.writing.Store(0)
	}

	for i := 0; i < 64; i++ {
		c.stats[i].hits.Store(0)
		c.stats[i].misses.Store(0)
	}
}

// Stats returns cache hit/miss statistics.
func (c *LRUCache) Stats() (hits, misses int64, ratio float64) {
	for i := 0; i < 64; i++ {
		hits += c.stats[i].hits.Load()
		misses += c.stats[i].misses.Load()
	}
	total := hits + misses
	if total > 0 {
		ratio = float64(hits) / float64(total)
	}
	return
}
