// Package liteLRU implements a high-performance, contention-bounded set-associative cache
// combining off-heap mmap chunked bitmask synchronization with immutable Go-heap entries.
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

// cacheEntry holds an immutable snapshot of a key and its associated payload.
// Once published via an atomic.Pointer store, fields are never mutated, enabling
// untorn, seqlock-free reads while keeping fixed slot metadata at 8 bytes/slot on heap.
type cacheEntry struct {
	method  string
	path    string
	handler HandlerFunc
	params  []Param
}

// HashRoute mixes method and path into a deterministic 64-bit hash using
// word-at-a-time mixing with zero heap allocations.
func HashRoute(method, path string) uint64 {
	const (
		k1 = uint64(0x9e3779b97f4a7c15)
		k2 = uint64(0xbf58476d1ce4e5b9)
		k3 = uint64(0x94d049bb133111eb)
	)
	h := uint64(len(method)+len(path)) * k1
	h = mixString(h^k2, method)
	h = mixString(bits.RotateLeft64(h, 27)^k3, path)
	h ^= h >> 30
	h *= k2
	h ^= h >> 27
	h *= k3
	h ^= h >> 31
	return h
}

func hashRoute(method, path string) uint64 {
	return HashRoute(method, path)
}

func mixString(h uint64, s string) uint64 {
	const (
		k1 = uint64(0x9e3779b97f4a7c15)
		k2 = uint64(0xbf58476d1ce4e5b9)
	)
	n := len(s)
	p := unsafe.StringData(s)
	i := 0
	for ; i+8 <= n; i += 8 {
		w := *(*uint64)(unsafe.Add(unsafe.Pointer(p), i))
		h ^= w * k1
		h = bits.RotateLeft64(h, 29) * k2
	}
	if i+4 <= n {
		w := uint64(*(*uint32)(unsafe.Add(unsafe.Pointer(p), i)))
		h ^= w * k1
		h = bits.RotateLeft64(h, 19) * k2
		i += 4
	}
	for ; i < n; i++ {
		h ^= uint64(*(*byte)(unsafe.Add(unsafe.Pointer(p), i)))
		h *= 1099511628211
	}
	return h
}

func computeSig8(hash uint64) uint8 {
	sig8 := uint8(hash >> 32)
	if sig8 == 0 {
		return 1
	}
	return sig8
}

// chunk represents a 64-slot set-associative unit packed cleanly into 128 bytes:
//   - [0..64):   sigs[8] (64 8-bit SWAR signatures, isolated in first 64B cache line)
//   - [64..128): valid, prob (Small/Main S3-FIFO partition), 2-bit frequency masks (f0, f1),
//     exclusive writer lock mask (writing), 128-bit evicted Ghost Bloom filter
//     (ghost0, ghost1), and rotating Small/Main eviction hands (hand, handM).
type chunk struct {
	sigs    [8]atomic.Uint64
	valid   atomic.Uint64
	prob    atomic.Uint64
	f0      atomic.Uint64
	f1      atomic.Uint64
	writing atomic.Uint64
	ghost0  atomic.Uint64
	ghost1  atomic.Uint64
	hand    atomic.Uint32
	handM   atomic.Uint32
}

// statStripe shards cache counters across cache-line-aligned slots indexed by goroutine stack.
type statStripe struct {
	hits   atomic.Int64
	misses atomic.Int64
	drops  atomic.Int64
	_      [CacheLineSize - 24]byte
}

// LRUCache implements a contention-bounded 64-way set-associative cache backed by
// off-heap mmap Bitmask S3-FIFO control chunks and immutable single-pointer Go-heap entries.
type LRUCache struct {
	capacity  uint32
	maxParams int

	entries []atomic.Pointer[cacheEntry]

	chunks     []chunk
	chunksSlab []byte

	numGroups uint32
	stats     [128]statStripe
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

// mmapSlice allocates an off-heap slab of n elements of T via MmapAnonymous,
// falling back to a Go slice if mmap is unavailable.
func mmapSlice[T any](n int) ([]T, []byte) {
	var zero T
	size := int(unsafe.Sizeof(zero)) * n
	slab, err := memory.MmapAnonymous(size)
	if err != nil {
		return make([]T, n), nil
	}
	return unsafe.Slice((*T)(unsafe.Pointer(unsafe.SliceData(slab))), n), slab
}

// NewLRUCache creates a new lock-free 64-way set-associative Bitmask S3-FIFO cache.
// Control metadata lives in off-heap mmap chunks (128 bytes per 64 slots = 2 bytes/slot),
// while immutable entry snapshots are referenced via a single atomic.Pointer array on Go heap.
// Call Close() to unmap off-heap slabs when the cache is no longer needed.
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
	entries := make([]atomic.Pointer[cacheEntry], capacity)
	chunks, chunksSlab := mmapSlice[chunk](int(numGroups))

	return &LRUCache{
		capacity:   uint32(capacity),
		maxParams:  maxParams,
		entries:    entries,
		chunks:     chunks,
		chunksSlab: chunksSlab,
		numGroups:  numGroups,
	}
}

// Close releases all off-heap mmap slabs. The cache must not be used after Close.
func (c *LRUCache) Close() {
	if c.chunksSlab != nil {
		memory.Munmap(c.chunksSlab)
		c.chunksSlab = nil
	}
}

//go:nosplit
func workerStripe() uint {
	var anchor uintptr
	sp := uintptr(unsafe.Pointer(&anchor))
	return uint(((sp >> 11) * 0x9e3779b97f4a7c15) >> 57) // 0..127 across 2KB-aligned stacks
}

func pickFromHand(cand uint64, hand uint32) uint32 {
	if above := cand & (^uint64(0) << (hand & 63)); above != 0 {
		return uint32(bits.TrailingZeros64(above))
	}
	return uint32(bits.TrailingZeros64(cand))
}

func (c *LRUCache) recordGhost(chk *chunk, evictedHash uint64) {
	b0 := uint64(1) << (uint(evictedHash>>40) & 63)
	b1 := uint64(1) << (uint(evictedHash>>48) & 63)
	if bits.OnesCount64(chk.ghost0.Load()) >= 34 {
		chk.ghost0.Store(b0)
		chk.ghost1.Store(b1)
		return
	}
	chk.ghost0.Or(b0)
	chk.ghost1.Or(b1)
}

func (c *LRUCache) inGhost(chk *chunk, h uint64) bool {
	b0 := uint64(1) << (uint(h>>40) & 63)
	b1 := uint64(1) << (uint(h>>48) & 63)
	return (chk.ghost0.Load()&b0) != 0 && (chk.ghost1.Load()&b1) != 0
}

// promoteFreq atomically increments a slot's 2-bit thermometer frequency (0->1->2)
// only if not already saturated, avoiding dirty cache-line writes on hot reads.
func promoteFreq(chk *chunk, mask uint64) {
	if (chk.f0.Load() & mask) == 0 {
		chk.f0.Or(mask)
	} else if (chk.f1.Load() & mask) == 0 {
		chk.f1.Or(mask)
	}
}

// findVictim uses Bitmask S3-FIFO within a 64-slot set to claim an unowned victim slot
// under exclusive W_k ownership, shedding load after 10 failed retries.
func (c *LRUCache) findVictim(group uint32) uint32 {
	chk := &c.chunks[group]
	const smallTarget = 4

	for retries := 0; retries < 10; retries++ {
		validBits := chk.valid.Load()
		writingBits := chk.writing.Load()
		avail := ^writingBits

		// 1. Empty slots first
		if empty := ^validBits & avail; empty != 0 {
			bit := uint32(bits.TrailingZeros64(empty))
			if chk.writing.CompareAndSwap(writingBits, writingBits|(uint64(1)<<bit)) {
				return group*64 + bit
			}
			continue
		}

		probBits := chk.prob.Load() & validBits
		f0 := chk.f0.Load()

		// 2. Bulk-promote all Small entries that received at least 1 hit into Main (f0=0, f1=0).
		if hitSmall := probBits & f0; hitSmall != 0 {
			chk.prob.And(^hitSmall)
			chk.f0.And(^hitSmall)
			chk.f1.And(^hitSmall)
			probBits &= ^hitSmall
		}

		// 3. Evict from Small FIFO when probationary occupancy meets target.
		if bits.OnesCount64(probBits) >= smallTarget {
			if coldSmall := probBits & ^f0 & avail; coldSmall != 0 {
				bit := pickFromHand(coldSmall, chk.hand.Load())
				if chk.writing.CompareAndSwap(writingBits, writingBits|(uint64(1)<<bit)) {
					chk.hand.Store((bit + 1) & 63)
					return group*64 + bit
				}
				continue
			}
		}

		// 4. Evict from Main using frequency-decrementing CLOCK across ^prob slots.
		mainBits := validBits & ^probBits
		if mainBits != 0 {
			f0 = chk.f0.Load()
			if coldMain := mainBits & ^f0 & avail; coldMain != 0 {
				bit := pickFromHand(coldMain, chk.handM.Load())
				if chk.writing.CompareAndSwap(writingBits, writingBits|(uint64(1)<<bit)) {
					chk.handM.Store((bit + 1) & 63)
					return group*64 + bit
				}
				continue
			}
			// Age ONLY Main entries in O(1): (f1,f0) -> (0,f1)
			f1 := chk.f1.Load()
			chk.f0.And(^mainBits | f1)
			chk.f1.And(^mainBits)
		} else if cold := validBits & avail; cold != 0 {
			// Rare fallback if Small holds all 64 slots simultaneously.
			chk.f0.Store(0)
			bit := pickFromHand(cold, chk.hand.Load())
			if chk.writing.CompareAndSwap(writingBits, writingBits|(uint64(1)<<bit)) {
				chk.hand.Store((bit + 1) & 63)
				return group*64 + bit
			}
			continue
		}

		if writingBits != 0 {
			runtime.Gosched()
		}
	}
	return 0xFFFFFFFF
}

// Add performs a best-effort, non-linearizable cache insertion or in-place update.
//
// Concurrency and progress contract:
//   - Concurrent cold misses on the same key may insert duplicate valid entries into
//     distinct slots within the same 64-way set; Get returns the lowest-index match,
//     so a later-completed Add does not necessarily supersede an earlier one, and evicting
//     an earlier slot can resurface an older duplicate.
//   - Per-slot ownership acquisition (W_k) is bounded to R=10 CAS retries per matched
//     fast-path slot and per findVictim eviction scan, shedding load (drops++) rather than
//     spinning unboundedly under contention; post-claim signature-word CAS updates
//     (chk.sigs) retry outside the R=10 budget under intra-word contention.
//   - Each insertion or update allocates a single immutable cacheEntry (plus a fresh
//     []Param slice if non-empty) published via single-word atomic.Pointer.Store.
//
// Callers requiring strict linearizable upserts over mutable values must synchronize externally.
func (c *LRUCache) Add(method, path string, handler HandlerFunc, params []Param) {
	hash := hashRoute(method, path)
	group := uint32(hash & uint64(c.numGroups-1))
	chk := &c.chunks[group]
	sig8 := computeSig8(hash)
	pattern := uint64(sig8) * 0x0101010101010101

	var copiedParams []Param
	if len(params) > 0 {
		copiedParams = make([]Param, len(params))
		copy(copiedParams, params)
	}
	newEntry := &cacheEntry{
		method:  method,
		path:    path,
		handler: handler,
		params:  copiedParams,
	}

	// 1. Fast-path update of an existing entry under exclusive W_k ownership.
	if validBits := chk.valid.Load(); validBits != 0 {
		for i := uint32(0); i < 8; i++ {
			vByte := uint8(validBits >> (i * 8))
			if vByte == 0 {
				continue
			}
			word := chk.sigs[i].Load()
			v := word ^ pattern
			matches := (v - 0x0101010101010101) &^ v & 0x8080808080808080
			for matches != 0 {
				tz := bits.TrailingZeros64(matches)
				matches &= matches - 1
				j := uint32(tz >> 3)
				if (vByte&(1<<j)) == 0 || uint8(word>>(j*8)) != sig8 {
					continue
				}
				bit := i*8 + j
				idx := group*64 + bit
				ep := c.entries[idx].Load()
				if ep == nil || ep.method != method || ep.path != path {
					continue
				}

				mask := uint64(1) << bit
				for retries := 0; ; retries++ {
					w := chk.writing.Load()
					if (w & mask) != 0 {
						c.stats[workerStripe()].drops.Add(1)
						return
					}
					if chk.writing.CompareAndSwap(w, w|mask) {
						break
					}
					if retries+1 >= 10 {
						c.stats[workerStripe()].drops.Add(1)
						return
					}
				}

				cur := c.entries[idx].Load()
				if (chk.valid.Load()&mask) == 0 || cur == nil || cur.method != method || cur.path != path {
					chk.writing.And(^mask)
					continue
				}

				c.entries[idx].Store(newEntry)
				promoteFreq(chk, mask)
				chk.writing.And(^mask)
				return
			}
		}
	}

	// 2. Miss: claim a victim slot in this 64-way set via Bitmask S3-FIFO.
	victimIdx := c.findVictim(group)
	if victimIdx == 0xFFFFFFFF {
		c.stats[workerStripe()].drops.Add(1)
		return
	}
	bit := victimIdx % 64
	mask := uint64(1) << bit

	chk.valid.And(^mask)
	if old := c.entries[victimIdx].Load(); old != nil && (chk.prob.Load()&mask) != 0 {
		// Unhit Small eviction: record hash into per-chunk Ghost Bloom filter.
		c.recordGhost(chk, hashRoute(old.method, old.path))
	}
	c.entries[victimIdx].Store(newEntry)

	sigWordIdx := bit / 8
	sigByteShift := (bit % 8) * 8
	for {
		oldWord := chk.sigs[sigWordIdx].Load()
		newWord := (oldWord & ^(uint64(0xFF) << sigByteShift)) | (uint64(sig8) << sigByteShift)
		if chk.sigs[sigWordIdx].CompareAndSwap(oldWord, newWord) {
			break
		}
	}

	if c.inGhost(chk, hash) {
		chk.prob.And(^mask) // Ghost hit: admit directly to Main
	} else {
		chk.prob.Or(mask) // Cold miss: admit to Small probationary queue
	}
	chk.f0.And(^mask)
	chk.f1.And(^mask)

	chk.valid.Or(mask)
	chk.writing.And(^mask)
}

// Get retrieves an entry from the cache via lock-free single-pointer immutable snapshots,
// incurring zero heap allocations when cap(dst) >= len(params) (including nil when len(params) == 0).
// Because key matching completes before dst is written and entries are immutable after publication,
// dst is never mutated on a failed lookup or miss.
func (c *LRUCache) Get(method, path string, dst []Param) (HandlerFunc, []Param, bool) {
	hash := hashRoute(method, path)
	group := uint32(hash & uint64(c.numGroups-1))
	chk := &c.chunks[group]

	validBits := chk.valid.Load()
	if validBits == 0 {
		c.stats[workerStripe()].misses.Add(1)
		return nil, nil, false
	}

	sig8 := computeSig8(hash)
	pattern := uint64(sig8) * 0x0101010101010101

	for i := uint32(0); i < 8; i++ {
		vByte := uint8(validBits >> (i * 8))
		if vByte == 0 {
			continue
		}
		word := chk.sigs[i].Load()
		v := word ^ pattern
		matches := (v - 0x0101010101010101) &^ v & 0x8080808080808080
		for matches != 0 {
			tz := bits.TrailingZeros64(matches)
			matches &= matches - 1
			j := uint32(tz >> 3)
			if (vByte&(1<<j)) == 0 || uint8(word>>(j*8)) != sig8 {
				continue
			}
			bit := i*8 + j
			idx := group*64 + bit
			ep := c.entries[idx].Load()
			if ep == nil || ep.method != method || ep.path != path {
				continue
			}

			var copiedParams []Param
			if len(ep.params) > 0 {
				if cap(dst) >= len(ep.params) {
					copiedParams = dst[:len(ep.params)]
				} else {
					copiedParams = make([]Param, len(ep.params))
				}
				copy(copiedParams, ep.params)
			}

			promoteFreq(chk, uint64(1)<<bit)
			c.stats[workerStripe()].hits.Add(1)
			return ep.handler, copiedParams, true
		}
	}

	c.stats[workerStripe()].misses.Add(1)
	return nil, nil, false
}

// Clear removes all entries from the cache chunk-by-chunk (not globally atomic across chunks).
//
// For each 64-slot chunk, Clear waits for W_k == 0 via runtime.Gosched(), claims all 64 ownership
// bits via CompareAndSwap(0, ^uint64(0)), clears bitmasks and signatures atomically, clears entry
// pointers for GC reclamation, and releases W_k. Concurrent writes can starve Clear().
func (c *LRUCache) Clear() {
	for group := uint32(0); group < c.numGroups; group++ {
		chk := &c.chunks[group]

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
		chk.prob.Store(0)
		chk.f0.Store(0)
		chk.f1.Store(0)
		chk.ghost0.Store(0)
		chk.ghost1.Store(0)
		chk.hand.Store(0)
		chk.handM.Store(0)
		for i := 0; i < 8; i++ {
			chk.sigs[i].Store(0)
		}

		for bit := uint32(0); bit < 64; bit++ {
			c.entries[group*64+bit].Store(nil)
		}

		chk.writing.Store(0)
	}

	for i := range c.stats {
		c.stats[i].hits.Store(0)
		c.stats[i].misses.Store(0)
		c.stats[i].drops.Store(0)
	}
}

// Stats returns aggregated cache hit/miss/drop statistics across worker stripes.
func (c *LRUCache) Stats() (hits, misses, drops int64, ratio float64) {
	for i := range c.stats {
		hits += c.stats[i].hits.Load()
		misses += c.stats[i].misses.Load()
		drops += c.stats[i].drops.Load()
	}
	total := hits + misses
	if total > 0 {
		ratio = float64(hits) / float64(total)
	}
	return hits, misses, drops, ratio
}
