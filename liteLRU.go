// Package liteLRU implements a high-performance LRU (Least Recently Used) cache
// designed for HTTP routing. It provides thread-safe operations and a fully
// lock-free, zero-tombstone 64-way set associative architecture backed by
// off-heap mmap memory, invisible to the Go garbage collector.
package liteLRU

import (
	"math/bits"
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

func hasByteSWAR(word uint64, b uint8) bool {
	pattern := uint64(b) * 0x0101010101010101
	v := word ^ pattern
	return (v-0x0101010101010101)&^v&0x8080808080808080 != 0
}

// chunk represents a group of 64 slots with bitmasks for O(1) eviction logic.
// Padded to 128 bytes (2 cache lines) to prevent false sharing.
type chunk struct {
	valid    atomic.Uint64
	accessed atomic.Uint64
	writing  atomic.Uint64
	_        [8]byte
	sigs     [8]atomic.Uint64 // 64 8-bit hash signatures (1 per slot)
	_        [32]byte         // pad to 128 bytes total
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

// LRUCache implements an ultra-low latency lock-free 64-way set associative cache.
// It uses a Chunked Bitmask CLOCK algorithm for O(1) eviction within each set,
// SIMD/SWAR byte scanning for O(1) lookups without a hash map, and off-heap
// mmap-backed SoA arrays that are invisible to the Go garbage collector.
type LRUCache struct {
	capacity  uint32
	maxParams int

	// Structure of Arrays (SoA) allocated on the Go heap so the GC can safely manage
	// dynamic strings and slice pointers.
	methods  []atomicString
	paths    []atomicString
	handlers []atomicHandler
	params   []atomicSlice

	// Concurrency control structures (no pointers), backed by off-heap mmap memory.
	// This avoids GC write barriers and scanning overhead during dense bitmask/seqlock operations.
	states []slotState // padded seqlocks to prevent read-tearing
	chunks []chunk     // padded bitmasks and SWAR signatures (64 slots per chunk)

	// Raw mmap slabs — held for Munmap on Close()
	statesSlab []byte
	chunksSlab []byte

	numGroups uint32
	stats     [64]statStripe
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

// mmapSlice allocates a contiguous off-heap slab of n elements of type T via
// MmapAnonymous and reinterprets it as []T. Falls back to make() if mmap fails
// (e.g., unsupported OS) so the cache is always functional.
func mmapSlice[T any](n int) ([]T, []byte) {
	var zero T
	size := int(unsafe.Sizeof(zero)) * n
	slab, err := memory.MmapAnonymous(size)
	if err != nil {
		// Graceful fallback: on unsupported platforms just use the heap.
		return make([]T, n), nil
	}
	return unsafe.Slice((*T)(unsafe.Pointer(unsafe.SliceData(slab))), n), slab
}

// NewLRUCache creates a new fully lock-free 64-way set associative LRU cache
// backed by off-heap mmap memory. The SoA arrays are invisible to the Go GC —
// no write barriers, no mark-phase scanning, no GC-induced tail latency.
// Call Close() to release the mmap slabs when the cache is no longer needed.
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

	numGroups := uint32(capacity / 64)

	methods := make([]atomicString, capacity)
	paths := make([]atomicString, capacity)
	handlers := make([]atomicHandler, capacity)
	params := make([]atomicSlice, capacity)

	states, statesSlab := mmapSlice[slotState](capacity)
	chunks, chunksSlab := mmapSlice[chunk](int(numGroups))

	return &LRUCache{
		capacity:   uint32(capacity),
		maxParams:  maxParams,
		methods:    methods,
		paths:      paths,
		handlers:   handlers,
		params:     params,
		states:     states,
		chunks:     chunks,
		statesSlab: statesSlab,
		chunksSlab: chunksSlab,
		numGroups:  numGroups,
	}
}

// Close releases all off-heap mmap slabs. The cache must not be used after Close.
func (c *LRUCache) Close() {
	for _, slab := range [][]byte{
		c.statesSlab, c.chunksSlab,
	} {
		if slab != nil {
			memory.Munmap(slab)
		}
	}
}

// findVictim uses bitwise operations to instantly find an eviction victim in O(1) time
// within a specific 64-slot set (group).
func (c *LRUCache) findVictim(group uint32) uint32 {
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
			continue // CAS failed, retry
		}

		// No candidates available in this chunk.
		// Clear the accessed bits of currently valid items to give them a second chance,
		// while preserving any concurrent access bits set by readers.
		chk.accessed.And(^validBits)
	}
}

// Add adds a new entry to the cache or updates an existing one.
func (c *LRUCache) Add(method, path string, handler HandlerFunc, params []Param) {
	hash := hashRoute(method, path)
	group := uint32(hash % uint64(c.numGroups))
	chk := &c.chunks[group]

	sig8 := uint8(hash >> 32)
	if sig8 == 0 {
		sig8 = 1
	}

	// 1. Try to find and update an existing entry
	for i := uint32(0); i < 8; i++ {
		word := chk.sigs[i].Load()
		if hasByteSWAR(word, sig8) {
			for j := uint32(0); j < 8; j++ {
				if byte((word>>(j*8))&0xFF) == sig8 {
					idx := group*64 + i*8 + j

					validBits := chk.valid.Load()
					if (validBits & (1 << (i*8 + j))) == 0 {
						continue
					}

					// Verify lock-free
					if c.methods[idx].Load() == method && c.paths[idx].Load() == path {
						// Found it! Try to lock and overwrite.
						seq := c.states[idx].seq.Load()
						if seq%2 != 0 || !c.states[idx].seq.CompareAndSwap(seq, seq+1) {
							return // Someone else is updating it, drop our redundant update
						}

						c.handlers[idx].Store(handler)
						oldParams := c.params[idx].Load()
						var newParams []Param
						if len(params) > 0 {
							if cap(oldParams) >= len(params) {
								newParams = oldParams[:len(params)]
								copy(newParams, params)
							} else {
								newParams = make([]Param, len(params))
								copy(newParams, params)
							}
						}
						c.params[idx].Store(newParams)

						c.states[idx].seq.Store(seq + 2)
						return
					}
				}
			}
		}
	}

	// 2. Not found, we need to evict a victim from this 64-slot set
	victimIdx := c.findVictim(group)
	bit := victimIdx % 64

	// We own the writing bit. Set seqlock to odd.
	seq := c.states[victimIdx].seq.Load()
	c.states[victimIdx].seq.Store(seq + 1) // odd

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
			newParams = make([]Param, len(params))
			copy(newParams, params)
		}
	} else {
		newParams = nil
	}
	c.params[victimIdx].Store(newParams)

	// Update SWAR signature
	sigWordIdx := bit / 8
	sigByteShift := (bit % 8) * 8
	for {
		oldWord := chk.sigs[sigWordIdx].Load()
		newWord := oldWord & ^(uint64(0xFF) << sigByteShift)
		newWord |= (uint64(sig8) << sigByteShift)
		if chk.sigs[sigWordIdx].CompareAndSwap(oldWord, newWord) {
			break
		}
	}

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

// Get retrieves an entry from the cache lock-free, zero allocation.
// The dst slice is used to avoid heap allocations when copying params.
func (c *LRUCache) Get(method, path string, dst []Param) (HandlerFunc, []Param, bool) {
	hash := hashRoute(method, path)
	group := uint32(hash % uint64(c.numGroups))
	chk := &c.chunks[group]
	stripeIdx := hash & 63

	sig8 := uint8(hash >> 32)
	if sig8 == 0 {
		sig8 = 1
	}

	for i := uint32(0); i < 8; i++ {
		word := chk.sigs[i].Load()
		if hasByteSWAR(word, sig8) {
			for j := uint32(0); j < 8; j++ {
				if byte((word>>(j*8))&0xFF) == sig8 {
					idx := group*64 + i*8 + j

					validBits := chk.valid.Load()
					if (validBits & (1 << (i*8 + j))) == 0 {
						continue
					}

					// Start read seqlock
					seq1 := c.states[idx].seq.Load()
					if seq1%2 != 0 {
						continue // Being written
					}

					// Validate method/path against concurrent evictions and collisions
					if c.methods[idx].Load() == method && c.paths[idx].Load() == path {
						// Safely read data
						handler := c.handlers[idx].Load()
						params := c.params[idx].Load()

						var copiedParams []Param
						if len(params) > 0 {
							if cap(dst) >= len(params) {
								copiedParams = dst[:len(params)]
							} else {
								copiedParams = make([]Param, len(params))
							}
							copy(copiedParams, params)
						}

						// Validate read seqlock
						seq2 := c.states[idx].seq.Load()
						if seq1 != seq2 {
							continue
						}

						// Mark as accessed for CLOCK via CAS loop
						bit := i*8 + j
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
				}
			}
		}
	}

	c.stats[stripeIdx].misses.Add(1)
	return nil, nil, false
}

// Clear gracefully removes all entries from the cache lock-free.
func (c *LRUCache) Clear() {
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

		// Clear signatures
		for i := 0; i < 8; i++ {
			chk.sigs[i].Store(0)
		}

		for bit := uint32(0); bit < 64; bit++ {
			idx := group*64 + bit
			c.params[idx].Store(nil)
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
