// Package liteLRU implements a high-performance LRU (Least Recently Used) cache
// designed for HTTP routing. It provides thread-safe operations and a fully
// lock-free, zero-tombstone 64-way set associative architecture backed by
// off-heap mmap memory, invisible to the Go garbage collector.
package liteLRU

import (
	"math/bits"
	"runtime"
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

// slotState holds the seqlock, padded to a full cache line
// to completely eliminate false-sharing during concurrent writes.
// Using uint64 makes counter wraparound unreachable in practice.
type slotState struct {
	seq atomic.Uint64
	_   [CacheLineSize - 8]byte
}

// statStripe shards cache statistics across independent cache lines
// to prevent global atomic contention during high-throughput parallel access.
type statStripe struct {
	hits   atomic.Int64
	misses atomic.Int64
	drops  atomic.Int64
	_      [CacheLineSize - 24]byte
}

// LRUCache implements an ultra-low latency lock-free 64-way set associative cache.
// It uses a Chunked Bitmask CLOCK algorithm for O(1) eviction within each set,
// SIMD/SWAR byte scanning for O(1) lookups without a hash map, and off-heap
// mmap-backed metadata arrays that are invisible to the Go garbage collector.
type LRUCache struct {
	capacity  uint32
	maxParams int

	// Structure of Arrays (SoA) allocated on the Go heap so the GC can safely manage
	// dynamic strings, closures, and slice pointers. Each slot holds an atomic.Pointer
	// to an immutable value published before releasing the seqlock.
	methods  []atomic.Pointer[string]
	paths    []atomic.Pointer[string]
	handlers []atomic.Pointer[HandlerFunc]
	params   []atomic.Pointer[[]Param]

	// Concurrency control structures (no pointers), backed by off-heap mmap memory.
	// This avoids GC write barriers and scanning overhead during dense bitmask/seqlock operations.
	states []slotState // padded seqlocks to prevent false sharing
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

// NewLRUCache creates a new lock-free 64-way set associative LRU cache
// whose synchronization metadata (seqlocks and chunk bitmasks/signatures) is
// backed by off-heap mmap memory, while pointer-bearing SoA payload slices remain
// on the Go heap for safe GC tracking.
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

	methods := make([]atomic.Pointer[string], capacity)
	paths := make([]atomic.Pointer[string], capacity)
	handlers := make([]atomic.Pointer[HandlerFunc], capacity)
	params := make([]atomic.Pointer[[]Param], capacity)

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

func computeSig8(hash uint64) uint8 {
	sig8 := uint8(hash >> 32)
	if sig8 == 0 {
		return 1
	}
	return sig8
}

func (c *LRUCache) slotKeyMatches(idx uint32, method, path string) bool {
	mp := c.methods[idx].Load()
	pp := c.paths[idx].Load()
	return mp != nil && pp != nil && *mp == method && *pp == path
}

// publishValue stores immutable handler and params snapshots for a slot while the
// caller holds exclusive slot ownership (W_k[bit] == 1) and an odd seqlock.
func (c *LRUCache) publishValue(idx uint32, handler HandlerFunc, params []Param) {
	if handler != nil {
		h := handler
		c.handlers[idx].Store(&h)
	} else {
		c.handlers[idx].Store(nil)
	}
	if len(params) > 0 {
		np := make([]Param, len(params))
		copy(np, params)
		c.params[idx].Store(&np)
	} else {
		c.params[idx].Store(nil)
	}
}

// findVictim uses bitwise operations to find and exclusively claim an eviction victim
// within a 64-slot set (group), shedding load after 10 failed attempts.
func (c *LRUCache) findVictim(group uint32) uint32 {
	chk := &c.chunks[group]
	retries := 0

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

			// Linearization point for exclusive slot ownership (W_k[bit]: 0 -> 1).
			if chk.writing.CompareAndSwap(writingBits, writingBits|(1<<bit)) {
				return group*64 + bit
			}

			retries++
			if retries >= 10 {
				return 0xFFFFFFFF // Shed load to guarantee bounded admission attempts
			}
			continue
		}

		// No candidates available in this chunk.
		// Clear the accessed bits of currently valid items to give them a second chance,
		// while preserving any concurrent access bits set on other slots.
		chk.accessed.And(^validBits)

		retries++
		if retries >= 10 {
			return 0xFFFFFFFF // Shed load to guarantee bounded admission attempts
		}

		if writingBits != 0 {
			runtime.Gosched()
		}
	}
}

// Add adds a new entry to the cache or performs a best-effort in-place update of an existing one.
func (c *LRUCache) Add(method, path string, handler HandlerFunc, params []Param) {
	hash := hashRoute(method, path)
	group := uint32(hash % uint64(c.numGroups))
	stripeIdx := hash & 63
	chk := &c.chunks[group]
	sig8 := computeSig8(hash)

	// 1. Try to find and update an existing entry under exclusive W_k ownership.
	for i := uint32(0); i < 8; i++ {
		word := chk.sigs[i].Load()
		if !hasByteSWAR(word, sig8) {
			continue
		}
		for j := uint32(0); j < 8; j++ {
			if byte((word>>(j*8))&0xFF) != sig8 {
				continue
			}
			bit := i*8 + j
			mask := uint64(1) << bit
			idx := group*64 + bit

			if (chk.valid.Load() & mask) == 0 {
				continue
			}

			seq1 := c.states[idx].seq.Load()
			if seq1&1 != 0 {
				continue
			}
			if !c.slotKeyMatches(idx, method, path) {
				continue
			}
			if c.states[idx].seq.Load() != seq1 {
				continue
			}

			// Acquire exclusive slot ownership via W_k before touching seq or payload.
			// Retry up to R=10 times if another slot in the same chunk toggled W_k;
			// drop immediately if this exact slot is already owned.
			for retries := 0; ; retries++ {
				w := chk.writing.Load()
				if (w & mask) != 0 {
					c.stats[stripeIdx].drops.Add(1)
					return // Another writer owns this exact slot; shed redundant update
				}
				if chk.writing.CompareAndSwap(w, w|mask) {
					break
				}
				if retries+1 >= 10 {
					c.stats[stripeIdx].drops.Add(1)
					return
				}
			}

			// Re-validate under exclusive W_k ownership that the slot was not evicted/cleared.
			if (chk.valid.Load()&mask) == 0 || !c.slotKeyMatches(idx, method, path) {
				chk.writing.And(^mask)
				continue
			}

			seq := c.states[idx].seq.Load()
			c.states[idx].seq.Store(seq + 1) // odd (WRITING)
			c.publishValue(idx, handler, params)
			if (chk.accessed.Load() & mask) == 0 {
				chk.accessed.Or(mask)
			}
			c.states[idx].seq.Store(seq + 2) // even (VALID)
			chk.writing.And(^mask)
			return
		}
	}

	// 2. Not found, claim a victim slot from this 64-slot set.
	victimIdx := c.findVictim(group)
	if victimIdx == 0xFFFFFFFF {
		c.stats[stripeIdx].drops.Add(1)
		return // Load shedding: chunk is contended, drop admission
	}
	bit := victimIdx % 64
	mask := uint64(1) << bit

	// Transition VALID/EMPTY -> INVALID -> WRITING while holding W_k[bit] == 1.
	chk.valid.And(^mask)
	seq := c.states[victimIdx].seq.Load()
	c.states[victimIdx].seq.Store(seq + 1) // odd (WRITING)

	m := method
	p := path
	c.methods[victimIdx].Store(&m)
	c.paths[victimIdx].Store(&p)
	c.publishValue(victimIdx, handler, params)

	// Update SWAR signature byte for slot bit.
	sigWordIdx := bit / 8
	sigByteShift := (bit % 8) * 8
	for {
		oldWord := chk.sigs[sigWordIdx].Load()
		newWord := (oldWord & ^(uint64(0xFF) << sigByteShift)) | (uint64(sig8) << sigByteShift)
		if chk.sigs[sigWordIdx].CompareAndSwap(oldWord, newWord) {
			break
		}
	}

	// Newly admitted entry receives CLOCK second-chance bit before publication.
	chk.accessed.Or(mask)
	c.states[victimIdx].seq.Store(seq + 2) // even (generation complete)
	chk.valid.Or(mask)                     // linearization point for VALID visibility
	chk.writing.And(^mask)                 // release exclusive writer ownership
}

// Get retrieves an entry from the cache lock-free with zero heap allocations
// when cap(dst) >= len(params).
func (c *LRUCache) Get(method, path string, dst []Param) (HandlerFunc, []Param, bool) {
	hash := hashRoute(method, path)
	group := uint32(hash % uint64(c.numGroups))
	chk := &c.chunks[group]
	stripeIdx := hash & 63
	sig8 := computeSig8(hash)

	for i := uint32(0); i < 8; i++ {
		word := chk.sigs[i].Load()
		if !hasByteSWAR(word, sig8) {
			continue
		}
		for j := uint32(0); j < 8; j++ {
			if byte((word>>(j*8))&0xFF) != sig8 {
				continue
			}
			bit := i*8 + j
			mask := uint64(1) << bit
			idx := group*64 + bit

			if (chk.valid.Load() & mask) == 0 {
				continue
			}

			seq1 := c.states[idx].seq.Load()
			if seq1&1 != 0 {
				continue // Being written
			}

			if !c.slotKeyMatches(idx, method, path) {
				continue
			}

			var handler HandlerFunc
			if hp := c.handlers[idx].Load(); hp != nil {
				handler = *hp
			}

			var copiedParams []Param
			if pp := c.params[idx].Load(); pp != nil && len(*pp) > 0 {
				src := *pp
				if cap(dst) >= len(src) {
					copiedParams = dst[:len(src)]
				} else {
					copiedParams = make([]Param, len(src))
				}
				copy(copiedParams, src)
			}

			if c.states[idx].seq.Load() != seq1 {
				continue
			}

			// Mark as accessed for CLOCK while avoiding unnecessary cache-line writes on hot hits.
			if (chk.accessed.Load() & mask) == 0 {
				chk.accessed.Or(mask)
			}

			c.stats[stripeIdx].hits.Add(1)
			return handler, copiedParams, true
		}
	}

	c.stats[stripeIdx].misses.Add(1)
	return nil, nil, false
}

// Clear gracefully removes all entries from the cache.
func (c *LRUCache) Clear() {
	for group := uint32(0); group < c.numGroups; group++ {
		chk := &c.chunks[group]

		// Wait until no concurrent writers hold any slot in this chunk, then claim all 64 slots.
		for {
			w := chk.writing.Load()
			if w != 0 {
				runtime.Gosched()
				continue
			}
			if chk.writing.CompareAndSwap(0, ^uint64(0)) {
				break
			}
		}

		chk.valid.Store(0)
		chk.accessed.Store(0)
		for i := 0; i < 8; i++ {
			chk.sigs[i].Store(0)
		}

		for bit := uint32(0); bit < 64; bit++ {
			idx := group*64 + bit
			seq := c.states[idx].seq.Load()
			c.states[idx].seq.Store(seq + 1) // odd
			c.params[idx].Store(nil)
			c.methods[idx].Store(nil)
			c.paths[idx].Store(nil)
			c.handlers[idx].Store(nil)
			c.states[idx].seq.Store(seq + 2) // even; preserves monotonic generation count
		}

		chk.writing.Store(0)
	}

	for i := 0; i < 64; i++ {
		c.stats[i].hits.Store(0)
		c.stats[i].misses.Store(0)
		c.stats[i].drops.Store(0)
	}
}

// Stats returns cache hit/miss/drop statistics.
func (c *LRUCache) Stats() (hits, misses, drops int64, ratio float64) {
	for i := 0; i < 64; i++ {
		hits += c.stats[i].hits.Load()
		misses += c.stats[i].misses.Load()
		drops += c.stats[i].drops.Load()
	}
	total := hits + misses
	if total > 0 {
		ratio = float64(hits) / float64(total)
	}
	return
}
