package main

import (
	"container/list"
	"flag"
	"fmt"
	"math"
	"math/bits"
	"math/rand"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/maypok86/otter"
	"github.com/xDarkicex/liteLRU"
)

// --- Summary stats helpers ---

func medianAndIQR(vals []float64) (med, q1, q3 float64) {
	cp := append([]float64(nil), vals...)
	sort.Float64s(cp)
	n := len(cp)
	if n == 0 {
		return 0, 0, 0
	}
	if n%2 == 1 {
		med = cp[n/2]
	} else {
		med = (cp[n/2-1] + cp[n/2]) / 2
	}
	q1 = cp[n/4]
	q3 = cp[(3*n)/4]
	return med, q1, q3
}

func medianDur(vals []time.Duration) time.Duration {
	cp := append([]time.Duration(nil), vals...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}

// --- MutexLRU comparator ---

type mutexEntry struct {
	key string
	val any
}

type mutexLRU struct {
	sync.Mutex
	cap   int
	ll    *list.List
	cache map[string]*list.Element
}

func newMutexLRU(cap int) *mutexLRU {
	return &mutexLRU{cap: cap, ll: list.New(), cache: make(map[string]*list.Element, cap)}
}

func (m *mutexLRU) Get(k string) (any, bool) {
	m.Lock()
	defer m.Unlock()
	if ele, ok := m.cache[k]; ok {
		m.ll.MoveToFront(ele)
		return ele.Value.(*mutexEntry).val, true
	}
	return nil, false
}

func (m *mutexLRU) Add(k string, v any) {
	m.Lock()
	defer m.Unlock()
	if ele, ok := m.cache[k]; ok {
		m.ll.MoveToFront(ele)
		ele.Value.(*mutexEntry).val = v
		return
	}
	if m.ll.Len() >= m.cap {
		oldest := m.ll.Back()
		if oldest != nil {
			m.ll.Remove(oldest)
			delete(m.cache, oldest.Value.(*mutexEntry).key)
		}
	}
	ele := m.ll.PushFront(&mutexEntry{key: k, val: v})
	m.cache[k] = ele
}

// --- Map + RWMutex ablation comparator (set-assoc disabled, lock-based map) ---

type rwMapCache struct {
	mu   sync.RWMutex
	cap  int
	ll   *list.List
	data map[string]*list.Element
}

func newRWMapCache(cap int) *rwMapCache {
	return &rwMapCache{cap: cap, ll: list.New(), data: make(map[string]*list.Element, cap)}
}

func (c *rwMapCache) Get(k string) bool {
	c.mu.RLock()
	_, ok := c.data[k]
	c.mu.RUnlock()
	return ok
}

func (c *rwMapCache) Add(k string) {
	c.mu.Lock()
	if ele, ok := c.data[k]; ok {
		c.ll.MoveToFront(ele)
		c.mu.Unlock()
		return
	}
	if c.ll.Len() >= c.cap {
		if old := c.ll.Back(); old != nil {
			c.ll.Remove(old)
			delete(c.data, old.Value.(string))
		}
	}
	ele := c.ll.PushFront(k)
	c.data[k] = ele
	c.mu.Unlock()
}

// --- Hardened Ablation Variants (identical W_k + uint64 seqlock + atomic.Pointer protocol) ---

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

type paddedSlot struct {
	seq atomic.Uint64
	_   [120]byte
}

type paddedStripe struct {
	hits   atomic.Int64
	misses atomic.Int64
	drops  atomic.Int64
	_      [104]byte
}

type paddedChunk struct {
	valid    atomic.Uint64
	accessed atomic.Uint64
	writing  atomic.Uint64
	_        [8]byte
	sigs     [8]atomic.Uint64
	_        [32]byte
}

type unpaddedSlot struct {
	seq atomic.Uint64
}

type unpaddedStripe struct {
	hits   atomic.Int64
	misses atomic.Int64
	drops  atomic.Int64
}

type unpaddedChunk struct {
	valid    atomic.Uint64
	accessed atomic.Uint64
	writing  atomic.Uint64
	sigs     [8]atomic.Uint64
}

type noSWARCache struct {
	methods   []atomic.Pointer[string]
	paths     []atomic.Pointer[string]
	handlers  []atomic.Pointer[liteLRU.HandlerFunc]
	params    []atomic.Pointer[[]liteLRU.Param]
	states    []paddedSlot
	chunks    []paddedChunk
	numGroups uint32
	stats     [64]paddedStripe
}

func newNoSWARCache(cap int) *noSWARCache {
	ng := uint32(cap / 64)
	return &noSWARCache{
		methods:   make([]atomic.Pointer[string], cap),
		paths:     make([]atomic.Pointer[string], cap),
		handlers:  make([]atomic.Pointer[liteLRU.HandlerFunc], cap),
		params:    make([]atomic.Pointer[[]liteLRU.Param], cap),
		states:    make([]paddedSlot, cap),
		chunks:    make([]paddedChunk, ng),
		numGroups: ng,
	}
}

func (c *noSWARCache) Get(method, path string) bool {
	h := hashRoute(method, path)
	group := uint32(h % uint64(c.numGroups))
	chk := &c.chunks[group]
	stripe := h & 63
	for bit := uint32(0); bit < 64; bit++ {
		mask := uint64(1) << bit
		if (chk.valid.Load() & mask) == 0 {
			continue
		}
		idx := group*64 + bit
		seq1 := c.states[idx].seq.Load()
		if seq1&1 != 0 {
			continue
		}
		mp, pp := c.methods[idx].Load(), c.paths[idx].Load()
		if mp == nil || pp == nil || *mp != method || *pp != path {
			continue
		}
		_ = c.handlers[idx].Load()
		_ = c.params[idx].Load()
		if c.states[idx].seq.Load() != seq1 {
			continue
		}
		if (chk.accessed.Load() & mask) == 0 {
			chk.accessed.Or(mask)
		}
		c.stats[stripe].hits.Add(1)
		return true
	}
	c.stats[stripe].misses.Add(1)
	return false
}

func (c *noSWARCache) Add(method, path string) {
	h := hashRoute(method, path)
	group := uint32(h % uint64(c.numGroups))
	chk := &c.chunks[group]
	stripe := h & 63
	for bit := uint32(0); bit < 64; bit++ {
		mask := uint64(1) << bit
		if (chk.valid.Load() & mask) == 0 {
			continue
		}
		idx := group*64 + bit
		seq1 := c.states[idx].seq.Load()
		if seq1&1 != 0 {
			continue
		}
		mp, pp := c.methods[idx].Load(), c.paths[idx].Load()
		if mp == nil || pp == nil || *mp != method || *pp != path {
			continue
		}
		if c.states[idx].seq.Load() != seq1 {
			continue
		}
		for retries := 0; ; retries++ {
			w := chk.writing.Load()
			if (w & mask) != 0 {
				c.stats[stripe].drops.Add(1)
				return
			}
			if chk.writing.CompareAndSwap(w, w|mask) {
				break
			}
			if retries+1 >= 10 {
				c.stats[stripe].drops.Add(1)
				return
			}
		}
		mp2, pp2 := c.methods[idx].Load(), c.paths[idx].Load()
		if (chk.valid.Load()&mask) == 0 || mp2 == nil || pp2 == nil || *mp2 != method || *pp2 != path {
			chk.writing.And(^mask)
			continue
		}
		seq := c.states[idx].seq.Load()
		c.states[idx].seq.Store(seq + 1)
		if (chk.accessed.Load() & mask) == 0 {
			chk.accessed.Or(mask)
		}
		c.states[idx].seq.Store(seq + 2)
		chk.writing.And(^mask)
		return
	}

	// findVictim
	var victim uint32 = 0xFFFFFFFF
	for retries := 0; ; retries++ {
		v, a, w := chk.valid.Load(), chk.accessed.Load(), chk.writing.Load()
		cand := (^v | (v & ^a)) & ^w
		if cand != 0 {
			bit := uint32(bits.TrailingZeros64(cand))
			if chk.writing.CompareAndSwap(w, w|(1<<bit)) {
				victim = group*64 + bit
				break
			}
		} else {
			chk.accessed.And(^v)
			if w != 0 {
				runtime.Gosched()
			}
		}
		if retries+1 >= 10 {
			break
		}
	}
	if victim == 0xFFFFFFFF {
		c.stats[stripe].drops.Add(1)
		return
	}
	bit := victim % 64
	mask := uint64(1) << bit
	chk.valid.And(^mask)
	seq := c.states[victim].seq.Load()
	c.states[victim].seq.Store(seq + 1)
	m, p := method, path
	c.methods[victim].Store(&m)
	c.paths[victim].Store(&p)
	chk.accessed.Or(mask)
	c.states[victim].seq.Store(seq + 2)
	chk.valid.Or(mask)
	chk.writing.And(^mask)
}

type noPadOnHeapCache struct {
	methods   []atomic.Pointer[string]
	paths     []atomic.Pointer[string]
	handlers  []atomic.Pointer[liteLRU.HandlerFunc]
	params    []atomic.Pointer[[]liteLRU.Param]
	states    []unpaddedSlot
	chunks    []unpaddedChunk
	numGroups uint32
	stats     [64]unpaddedStripe
}

func newNoPadOnHeapCache(cap int) *noPadOnHeapCache {
	ng := uint32(cap / 64)
	return &noPadOnHeapCache{
		methods:   make([]atomic.Pointer[string], cap),
		paths:     make([]atomic.Pointer[string], cap),
		handlers:  make([]atomic.Pointer[liteLRU.HandlerFunc], cap),
		params:    make([]atomic.Pointer[[]liteLRU.Param], cap),
		states:    make([]unpaddedSlot, cap),
		chunks:    make([]unpaddedChunk, ng),
		numGroups: ng,
	}
}

func (c *noPadOnHeapCache) Get(method, path string) bool {
	h := hashRoute(method, path)
	group := uint32(h % uint64(c.numGroups))
	chk := &c.chunks[group]
	stripe := h & 63
	sig8 := uint8(h >> 32)
	if sig8 == 0 {
		sig8 = 1
	}
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
			if (chk.valid.Load() & mask) == 0 {
				continue
			}
			idx := group*64 + bit
			seq1 := c.states[idx].seq.Load()
			if seq1&1 != 0 {
				continue
			}
			mp, pp := c.methods[idx].Load(), c.paths[idx].Load()
			if mp == nil || pp == nil || *mp != method || *pp != path {
				continue
			}
			_ = c.handlers[idx].Load()
			_ = c.params[idx].Load()
			if c.states[idx].seq.Load() != seq1 {
				continue
			}
			if (chk.accessed.Load() & mask) == 0 {
				chk.accessed.Or(mask)
			}
			c.stats[stripe].hits.Add(1)
			return true
		}
	}
	c.stats[stripe].misses.Add(1)
	return false
}

func (c *noPadOnHeapCache) Add(method, path string) {
	h := hashRoute(method, path)
	group := uint32(h % uint64(c.numGroups))
	chk := &c.chunks[group]
	stripe := h & 63
	sig8 := uint8(h >> 32)
	if sig8 == 0 {
		sig8 = 1
	}
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
			if (chk.valid.Load() & mask) == 0 {
				continue
			}
			idx := group*64 + bit
			seq1 := c.states[idx].seq.Load()
			if seq1&1 != 0 {
				continue
			}
			mp, pp := c.methods[idx].Load(), c.paths[idx].Load()
			if mp == nil || pp == nil || *mp != method || *pp != path {
				continue
			}
			if c.states[idx].seq.Load() != seq1 {
				continue
			}
			for retries := 0; ; retries++ {
				w := chk.writing.Load()
				if (w & mask) != 0 {
					c.stats[stripe].drops.Add(1)
					return
				}
				if chk.writing.CompareAndSwap(w, w|mask) {
					break
				}
				if retries+1 >= 10 {
					c.stats[stripe].drops.Add(1)
					return
				}
			}
			mp2, pp2 := c.methods[idx].Load(), c.paths[idx].Load()
			if (chk.valid.Load()&mask) == 0 || mp2 == nil || pp2 == nil || *mp2 != method || *pp2 != path {
				chk.writing.And(^mask)
				continue
			}
			seq := c.states[idx].seq.Load()
			c.states[idx].seq.Store(seq + 1)
			if (chk.accessed.Load() & mask) == 0 {
				chk.accessed.Or(mask)
			}
			c.states[idx].seq.Store(seq + 2)
			chk.writing.And(^mask)
			return
		}
	}

	var victim uint32 = 0xFFFFFFFF
	for retries := 0; ; retries++ {
		v, a, w := chk.valid.Load(), chk.accessed.Load(), chk.writing.Load()
		cand := (^v | (v & ^a)) & ^w
		if cand != 0 {
			bit := uint32(bits.TrailingZeros64(cand))
			if chk.writing.CompareAndSwap(w, w|(1<<bit)) {
				victim = group*64 + bit
				break
			}
		} else {
			chk.accessed.And(^v)
			if w != 0 {
				runtime.Gosched()
			}
		}
		if retries+1 >= 10 {
			break
		}
	}
	if victim == 0xFFFFFFFF {
		c.stats[stripe].drops.Add(1)
		return
	}
	bit := victim % 64
	mask := uint64(1) << bit
	chk.valid.And(^mask)
	seq := c.states[victim].seq.Load()
	c.states[victim].seq.Store(seq + 1)
	m, p := method, path
	c.methods[victim].Store(&m)
	c.paths[victim].Store(&p)
	sigWordIdx := bit / 8
	sigShift := (bit % 8) * 8
	for {
		ow := chk.sigs[sigWordIdx].Load()
		nw := (ow & ^(uint64(0xFF) << sigShift)) | (uint64(sig8) << sigShift)
		if chk.sigs[sigWordIdx].CompareAndSwap(ow, nw) {
			break
		}
	}
	chk.accessed.Or(mask)
	c.states[victim].seq.Store(seq + 2)
	chk.valid.Or(mask)
	chk.writing.And(^mask)
}

// paddedOnHeapCache: identical to hardened liteLRU (128B padded), except states/chunks live on Go heap (no mmap).
type paddedOnHeapCache struct {
	methods   []atomic.Pointer[string]
	paths     []atomic.Pointer[string]
	states    []paddedSlot
	chunks    []paddedChunk
	numGroups uint32
	stats     [64]paddedStripe
}

func newPaddedOnHeapCache(cap int) *paddedOnHeapCache {
	ng := uint32(cap / 64)
	return &paddedOnHeapCache{
		methods:   make([]atomic.Pointer[string], cap),
		paths:     make([]atomic.Pointer[string], cap),
		states:    make([]paddedSlot, cap),
		chunks:    make([]paddedChunk, ng),
		numGroups: ng,
	}
}

func (c *paddedOnHeapCache) Get(method, path string) bool {
	h := hashRoute(method, path)
	group := uint32(h % uint64(c.numGroups))
	chk := &c.chunks[group]
	stripe := h & 63
	sig8 := uint8(h >> 32)
	if sig8 == 0 {
		sig8 = 1
	}
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
			if (chk.valid.Load() & mask) == 0 {
				continue
			}
			idx := group*64 + bit
			seq1 := c.states[idx].seq.Load()
			if seq1&1 != 0 {
				continue
			}
			mp, pp := c.methods[idx].Load(), c.paths[idx].Load()
			if mp == nil || pp == nil || *mp != method || *pp != path {
				continue
			}
			if c.states[idx].seq.Load() != seq1 {
				continue
			}
			if (chk.accessed.Load() & mask) == 0 {
				chk.accessed.Or(mask)
			}
			c.stats[stripe].hits.Add(1)
			return true
		}
	}
	c.stats[stripe].misses.Add(1)
	return false
}

func (c *paddedOnHeapCache) Add(method, path string) {
	h := hashRoute(method, path)
	group := uint32(h % uint64(c.numGroups))
	chk := &c.chunks[group]
	stripe := h & 63
	sig8 := uint8(h >> 32)
	if sig8 == 0 {
		sig8 = 1
	}
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
			if (chk.valid.Load() & mask) == 0 {
				continue
			}
			idx := group*64 + bit
			seq1 := c.states[idx].seq.Load()
			if seq1&1 != 0 {
				continue
			}
			mp, pp := c.methods[idx].Load(), c.paths[idx].Load()
			if mp == nil || pp == nil || *mp != method || *pp != path {
				continue
			}
			if c.states[idx].seq.Load() != seq1 {
				continue
			}
			for retries := 0; ; retries++ {
				w := chk.writing.Load()
				if (w & mask) != 0 {
					c.stats[stripe].drops.Add(1)
					return
				}
				if chk.writing.CompareAndSwap(w, w|mask) {
					break
				}
				if retries+1 >= 10 {
					c.stats[stripe].drops.Add(1)
					return
				}
			}
			mp2, pp2 := c.methods[idx].Load(), c.paths[idx].Load()
			if (chk.valid.Load()&mask) == 0 || mp2 == nil || pp2 == nil || *mp2 != method || *pp2 != path {
				chk.writing.And(^mask)
				continue
			}
			seq := c.states[idx].seq.Load()
			c.states[idx].seq.Store(seq + 1)
			if (chk.accessed.Load() & mask) == 0 {
				chk.accessed.Or(mask)
			}
			c.states[idx].seq.Store(seq + 2)
			chk.writing.And(^mask)
			return
		}
	}

	var victim uint32 = 0xFFFFFFFF
	for retries := 0; ; retries++ {
		v, a, w := chk.valid.Load(), chk.accessed.Load(), chk.writing.Load()
		cand := (^v | (v & ^a)) & ^w
		if cand != 0 {
			bit := uint32(bits.TrailingZeros64(cand))
			if chk.writing.CompareAndSwap(w, w|(1<<bit)) {
				victim = group*64 + bit
				break
			}
		} else {
			chk.accessed.And(^v)
			if w != 0 {
				runtime.Gosched()
			}
		}
		if retries+1 >= 10 {
			break
		}
	}
	if victim == 0xFFFFFFFF {
		c.stats[stripe].drops.Add(1)
		return
	}
	bit := victim % 64
	mask := uint64(1) << bit
	chk.valid.And(^mask)
	seq := c.states[victim].seq.Load()
	c.states[victim].seq.Store(seq + 1)
	m, p := method, path
	c.methods[victim].Store(&m)
	c.paths[victim].Store(&p)
	sigWordIdx := bit / 8
	sigShift := (bit % 8) * 8
	for {
		ow := chk.sigs[sigWordIdx].Load()
		nw := (ow & ^(uint64(0xFF) << sigShift)) | (uint64(sig8) << sigShift)
		if chk.sigs[sigWordIdx].CompareAndSwap(ow, nw) {
			break
		}
	}
	chk.accessed.Or(mask)
	c.states[victim].seq.Store(seq + 2)
	chk.valid.Or(mask)
	chk.writing.And(^mask)
}

var _ = unsafe.Sizeof(paddedChunk{})

func runWorkers(ops []string, workers int, getFn, addFn func(string)) (opsPerSec float64, p50, p99 time.Duration) {
	var wg sync.WaitGroup
	wg.Add(workers)
	perWorker := len(ops) / workers
	lat := make([][]time.Duration, workers)
	start := time.Now()
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			s := id * perWorker
			e := s + perWorker
			local := make([]time.Duration, 0, perWorker/100+1)
			for i := s; i < e; i++ {
				sample := i%100 == 0
				var t0 time.Time
				if sample {
					t0 = time.Now()
				}
				if i%5 != 0 {
					getFn(ops[i])
				} else {
					addFn(ops[i])
				}
				if sample {
					local = append(local, time.Since(t0))
				}
			}
			lat[id] = local
		}(w)
	}
	wg.Wait()
	dur := time.Since(start)
	var all []time.Duration
	for _, l := range lat {
		all = append(all, l...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	return float64(len(ops)) / dur.Seconds(), all[len(all)*50/100], all[len(all)*99/100]
}

func runNoSample(ops []string, workers int, getFn, addFn func(string)) float64 {
	var wg sync.WaitGroup
	wg.Add(workers)
	perWorker := len(ops) / workers
	start := time.Now()
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			s := id * perWorker
			e := s + perWorker
			for i := s; i < e; i++ {
				if i%5 != 0 {
					getFn(ops[i])
				} else {
					addFn(ops[i])
				}
			}
		}(w)
	}
	wg.Wait()
	return float64(len(ops)) / time.Since(start).Seconds()
}

func runNoSample5050(ops []string, workers int, getFn, addFn func(string)) float64 {
	var wg sync.WaitGroup
	wg.Add(workers)
	perWorker := len(ops) / workers
	start := time.Now()
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			s := id * perWorker
			e := s + perWorker
			for i := s; i < e; i++ {
				if i%2 == 0 {
					getFn(ops[i])
				} else {
					addFn(ops[i])
				}
			}
		}(w)
	}
	wg.Wait()
	return float64(len(ops)) / time.Since(start).Seconds()
}

func main() {
	onlyAblation := flag.Bool("only-ablation", false, "run only section 2 hardened ablation")
	flag.Parse()

	const runs = 10
	const numKeys = 1000
	const numOps = 1600000
	const capacity = 1024

	// Deterministic 80/20 uniform-pool workload (70% hot 200 keys, 30% 1000 keys).
	rng := rand.New(rand.NewSource(42))
	keys := make([]string, numKeys)
	for i := 0; i < numKeys; i++ {
		keys[i] = fmt.Sprintf("GET:/api/v1/resource/%d", i)
	}
	uniformOps := make([]string, numOps)
	for i := 0; i < numOps; i++ {
		if rng.Float64() < 0.7 {
			uniformOps[i] = keys[i%200]
		} else {
			uniformOps[i] = keys[i%numKeys]
		}
	}

	if !*onlyAblation {
		fmt.Println("=== 1. BASELINE (10 runs, median + IQR) ===")
		var lOps, oOps, mOps []float64
		var lp50, lp99, op50, op99, mp50, mp99 []time.Duration
		for r := 0; r < runs; r++ {
			lc := liteLRU.NewLRUCache(capacity, 10)
			ops, p50, p99 := runWorkers(uniformOps, 8, func(k string) { lc.Get("GET", k, nil) }, func(k string) { lc.Add("GET", k, nil, nil) })
			lc.Close()
			lOps = append(lOps, ops)
			lp50 = append(lp50, p50)
			lp99 = append(lp99, p99)

			oc, _ := otter.MustBuilder[string, any](capacity).Build()
			ops, p50, p99 = runWorkers(uniformOps, 8, func(k string) { oc.Get(k) }, func(k string) { oc.Set(k, 1) })
			oc.Close()
			oOps = append(oOps, ops)
			op50 = append(op50, p50)
			op99 = append(op99, p99)

			mc := newMutexLRU(capacity)
			ops, p50, p99 = runWorkers(uniformOps, 8, func(k string) { mc.Get(k) }, func(k string) { mc.Add(k, 1) })
			mOps = append(mOps, ops)
			mp50 = append(mp50, p50)
			mp99 = append(mp99, p99)
		}
		lm, lq1, lq3 := medianAndIQR(lOps)
		om, oq1, oq3 := medianAndIQR(oOps)
		mm, mq1, mq3 := medianAndIQR(mOps)
		fmt.Printf("liteLRU:   med=%.0f (IQR %.0f-%.0f) p50=%v p99=%v\n", lm, lq1, lq3, medianDur(lp50), medianDur(lp99))
		fmt.Printf("otter:     med=%.0f (IQR %.0f-%.0f) p50=%v p99=%v\n", om, oq1, oq3, medianDur(op50), medianDur(op99))
		fmt.Printf("MutexLRU:  med=%.0f (IQR %.0f-%.0f) p50=%v p99=%v\n", mm, mq1, mq3, medianDur(mp50), medianDur(mp99))
	}

	zRng := rand.New(rand.NewSource(99))
	zipf100k := rand.NewZipf(zRng, 1.001, 1, 99999)
	zipfOps := make([]string, numOps)
	for i := 0; i < numOps; i++ {
		zipfOps[i] = strconv.FormatUint(zipf100k.Uint64(), 10)
	}

	runAblation := func(label string, ops []string, runner func([]string, int, func(string), func(string)) float64) {
		fmt.Printf("\n--- %s ---\n", label)
		var aFull, aNoSWAR, aNoPad, aMutexLRU, aRWMap []float64
		for r := 0; r < runs; r++ {
			fc := liteLRU.NewLRUCache(capacity, 10)
			aFull = append(aFull, runner(ops, 8, func(k string) { fc.Get("GET", k, nil) }, func(k string) { fc.Add("GET", k, nil, nil) }))
			fc.Close()

			ns := newNoSWARCache(capacity)
			aNoSWAR = append(aNoSWAR, runner(ops, 8, func(k string) { ns.Get("GET", k) }, func(k string) { ns.Add("GET", k) }))

			np := newNoPadOnHeapCache(capacity)
			aNoPad = append(aNoPad, runner(ops, 8, func(k string) { np.Get("GET", k) }, func(k string) { np.Add("GET", k) }))

			mc := newMutexLRU(capacity)
			aMutexLRU = append(aMutexLRU, runner(ops, 8, func(k string) { mc.Get(k) }, func(k string) { mc.Add(k, 1) }))

			rw := newRWMapCache(capacity)
			aRWMap = append(aRWMap, runner(ops, 8, func(k string) { rw.Get(k) }, func(k string) { rw.Add(k) }))
		}
		fm, fq1, fq3 := medianAndIQR(aFull)
		nsm, nsq1, nsq3 := medianAndIQR(aNoSWAR)
		npm, npq1, npq3 := medianAndIQR(aNoPad)
		mlm, mlq1, mlq3 := medianAndIQR(aMutexLRU)
		rwm, rwq1, rwq3 := medianAndIQR(aRWMap)
		fmt.Printf("Full hardened: med=%.0f (IQR %.0f-%.0f)\n", fm, fq1, fq3)
		fmt.Printf("No SWAR:       med=%.0f (IQR %.0f-%.0f) [%.1f%%]\n", nsm, nsq1, nsq3, (nsm-fm)/fm*100)
		fmt.Printf("No Padding:    med=%.0f (IQR %.0f-%.0f) [%.1f%%]\n", npm, npq1, npq3, (npm-fm)/fm*100)
		fmt.Printf("True MutexLRU: med=%.0f (IQR %.0f-%.0f) [%.1f%%]\n", mlm, mlq1, mlq3, (mlm-fm)/fm*100)
		fmt.Printf("Map+RWMutex*:  med=%.0f (IQR %.0f-%.0f) [%.1f%%]\n", rwm, rwq1, rwq3, (rwm-fm)/fm*100)
	}

	fmt.Println("\n=== 2. HARDENED ABLATION (10 runs, median + IQR, no sampling) ===")
	runAblation("80/20 Uniform-Pool (1000 keys, cap=1024, low eviction)", uniformOps, runNoSample)
	runAblation("80/20 Zipfian (100k keys, cap=1024, active eviction)", zipfOps, runNoSample)
	runAblation("50/50 Zipfian (100k keys, cap=1024, write-heavy)", zipfOps, runNoSample5050)

	if *onlyAblation {
		return
	}

	fmt.Println("\n=== 3. ZIPFIAN SCALING (10 runs, s=1.001, N=100k, cap=1024) ===")
	var base1Core float64
	for _, cores := range []int{1, 2, 4, 8} {
		var vals []float64
		for r := 0; r < runs; r++ {
			c := liteLRU.NewLRUCache(capacity, 10)
			vals = append(vals, runNoSample(zipfOps, cores, func(k string) { c.Get("GET", k, nil) }, func(k string) { c.Add("GET", k, nil, nil) }))
			c.Close()
		}
		m, q1, q3 := medianAndIQR(vals)
		if cores == 1 {
			base1Core = m
		}
		fmt.Printf("%d cores: med=%.0f (IQR %.0f-%.0f) [%.2fx]\n", cores, m, q1, q3, m/base1Core)
	}

	fmt.Println("\n=== 4. WRITE-HEAVY ZIPF (10 runs, N=1000, cap=1024) ===")
	for _, mix := range []struct {
		name   string
		getPct int
	}{{"50/50", 50}, {"20/80", 20}} {
		type op struct {
			isGet bool
			k     string
		}
		zr := rand.New(rand.NewSource(42))
		z := rand.NewZipf(zr, 1.001, 1, 999)
		ks := make([]string, 1000)
		for i := range ks {
			ks[i] = strconv.Itoa(i)
		}
		wOps := make([]op, numOps)
		addCount := 0
		for i := range wOps {
			g := zr.Intn(100) < mix.getPct
			if !g {
				addCount++
			}
			wOps[i] = op{isGet: g, k: ks[z.Uint64()]}
		}
		var lAtt, lEff, lDropPct, lHit, oOpsW []float64
		for r := 0; r < runs; r++ {
			lc := liteLRU.NewLRUCache(capacity, 10)
			var wg sync.WaitGroup
			wg.Add(8)
			chunk := numOps / 8
			st := time.Now()
			for w := 0; w < 8; w++ {
				go func(s, e int) {
					defer wg.Done()
					for i := s; i < e; i++ {
						if wOps[i].isGet {
							lc.Get("GET", wOps[i].k, nil)
						} else {
							lc.Add("GET", wOps[i].k, nil, nil)
						}
					}
				}(w*chunk, (w+1)*chunk)
			}
			wg.Wait()
			dur := time.Since(st).Seconds()
			_, _, drops, ratio := lc.Stats()
			lc.Close()
			lAtt = append(lAtt, float64(numOps)/dur)
			lEff = append(lEff, float64(numOps-int(drops))/dur)
			lDropPct = append(lDropPct, float64(drops)/float64(addCount)*100)
			lHit = append(lHit, ratio*100)

			oc, _ := otter.MustBuilder[string, any](capacity).Build()
			wg.Add(8)
			st = time.Now()
			for w := 0; w < 8; w++ {
				go func(s, e int) {
					defer wg.Done()
					for i := s; i < e; i++ {
						if wOps[i].isGet {
							oc.Get(wOps[i].k)
						} else {
							oc.Set(wOps[i].k, nil)
						}
					}
				}(w*chunk, (w+1)*chunk)
			}
			wg.Wait()
			dur = time.Since(st).Seconds()
			oc.Close()
			oOpsW = append(oOpsW, float64(numOps)/dur)
		}
		am, aq1, aq3 := medianAndIQR(lAtt)
		em, eq1, eq3 := medianAndIQR(lEff)
		dm, _, _ := medianAndIQR(lDropPct)
		hm, _, _ := medianAndIQR(lHit)
		omw, oq1w, oq3w := medianAndIQR(oOpsW)
		fmt.Printf("%s: liteAtt=%.0f (%.0f-%.0f) | liteEff=%.0f (%.0f-%.0f) | drop=%.2f%% | hit=%.2f%% | otter=%.0f (%.0f-%.0f) | effAdv=%.2fx\n",
			mix.name, am, aq1, aq3, em, eq1, eq3, dm, hm, omw, oq1w, oq3w, em/omw)
	}

	fmt.Println("\n=== 5. ZIPF HIT RATE (10 runs, matched power-of-two caps, N=100k) ===")
	for _, skew := range []float64{1.001, 0.80} {
		const ws = 100000
		const total = 2000000
		const warm = 400000
		pmf := make([]float64, ws)
		var sum float64
		for i := 1; i <= ws; i++ {
			p := 1.0 / math.Pow(float64(i), skew)
			pmf[i-1] = p
			sum += p
		}
		cdf := make([]float64, ws)
		var csum float64
		for i := 0; i < ws; i++ {
			csum += pmf[i] / sum
			cdf[i] = csum
		}
		sr := rand.New(rand.NewSource(77))
		seqOps := make([]string, total)
		for i := 0; i < total; i++ {
			rv := sr.Float64()
			idx := sort.Search(ws, func(j int) bool { return cdf[j] >= rv })
			if idx >= ws {
				idx = ws - 1
			}
			seqOps[i] = strconv.Itoa(idx)
		}
		for _, cap := range []int{16384, 32768, 65536} {
			var lHR, oHR []float64
			for r := 0; r < runs; r++ {
				lc := liteLRU.NewLRUCache(cap, 5)
				for i := 0; i < warm; i++ {
					if _, _, ok := lc.Get("GET", seqOps[i], nil); !ok {
						lc.Add("GET", seqOps[i], nil, nil)
					}
				}
				var hits atomic.Uint64
				var wg sync.WaitGroup
				wg.Add(8)
				cs := (total - warm) / 8
				for w := 0; w < 8; w++ {
					go func(s, e int) {
						defer wg.Done()
						for i := s; i < e; i++ {
							if _, _, ok := lc.Get("GET", seqOps[i], nil); ok {
								hits.Add(1)
							} else {
								lc.Add("GET", seqOps[i], nil, nil)
							}
						}
					}(warm+w*cs, warm+(w+1)*cs)
				}
				wg.Wait()
				lc.Close()
				lHR = append(lHR, float64(hits.Load())/float64(total-warm)*100)

				oc, _ := otter.MustBuilder[string, any](cap).Build()
				for i := 0; i < warm; i++ {
					if !oc.Has(seqOps[i]) {
						oc.Set(seqOps[i], nil)
					}
				}
				var oHits atomic.Uint64
				wg.Add(8)
				for w := 0; w < 8; w++ {
					go func(s, e int) {
						defer wg.Done()
						for i := s; i < e; i++ {
							if _, ok := oc.Get(seqOps[i]); ok {
								oHits.Add(1)
							} else {
								oc.Set(seqOps[i], nil)
							}
						}
					}(warm+w*cs, warm+(w+1)*cs)
				}
				wg.Wait()
				oc.Close()
				oHR = append(oHR, float64(oHits.Load())/float64(total-warm)*100)
			}
			lmh, _, _ := medianAndIQR(lHR)
			omh, _, _ := medianAndIQR(oHR)
			fmt.Printf("s=%.3f cap=%d (%.1f%%): liteLRU=%.2f%% vs Otter=%.2f%%\n",
				skew, cap, float64(cap)/float64(ws)*100, lmh, omh)
		}
	}

	fmt.Println("\n=== 6. GC ISOLATION EXPERIMENT (65,536 slots, background allocation pressure) ===")
	measureGC := func(name string, useMmap bool) {
		runtime.GC()
		var mBefore, mAfter runtime.MemStats
		runtime.ReadMemStats(&mBefore)

		var offHeap *liteLRU.LRUCache
		var onHeap *paddedOnHeapCache
		if useMmap {
			offHeap = liteLRU.NewLRUCache(65536, 10)
			defer offHeap.Close()
		} else {
			onHeap = newPaddedOnHeapCache(65536)
		}

		// Pre-populate cache
		for i := 0; i < 65536; i++ {
			k := "/gc/" + strconv.Itoa(i)
			if useMmap {
				offHeap.Add("GET", k, nil, nil)
			} else {
				onHeap.Add("GET", k)
			}
		}

		// Run 8 workers doing cache ops + simulated request allocations so GC triggers repeatedly
		var wg sync.WaitGroup
		wg.Add(8)
		st := time.Now()
		for w := 0; w < 8; w++ {
			go func(id int) {
				defer wg.Done()
				var sink []byte
				for i := 0; i < 200000; i++ {
					k := "/gc/" + strconv.Itoa((id*997+i)%65536)
					if i%5 == 0 {
						if useMmap {
							offHeap.Add("GET", k, nil, nil)
						} else {
							onHeap.Add("GET", k)
						}
					} else {
						if useMmap {
							offHeap.Get("GET", k, nil)
						} else {
							onHeap.Get("GET", k)
						}
					}
					if i%8 == 0 {
						sink = make([]byte, 512)
						sink[0] = byte(i)
					}
				}
				runtime.KeepAlive(sink)
			}(w)
		}
		wg.Wait()
		dur := time.Since(st)
		runtime.ReadMemStats(&mAfter)
		numGC := mAfter.NumGC - mBefore.NumGC
		pauseMs := float64(mAfter.PauseTotalNs-mBefore.PauseTotalNs) / 1e6
		fmt.Printf("%-22s: ops/s=%9.0f | NumGC=%3d | PauseTotal=%.3fms | HeapObjects=%d | GCCPUFraction=%.5f\n",
			name, float64(1600000)/dur.Seconds(), numGC, pauseMs, mAfter.HeapObjects, mAfter.GCCPUFraction)
	}
	measureGC("Off-heap mmap metadata", true)
	measureGC("On-heap Go slice meta", false)
}
