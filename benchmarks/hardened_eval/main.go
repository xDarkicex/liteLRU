package main

import (
	"container/list"
	"flag"
	"fmt"
	"math"
	"math/bits"
	"math/rand"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/maypok86/otter"
	"github.com/xDarkicex/liteLRU"
	"github.com/xDarkicex/memory"
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

// --- Map + RWMutex ablation comparator ---

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
	defer c.mu.Unlock()
	if ele, ok := c.data[k]; ok {
		c.ll.MoveToFront(ele)
		return
	}
	if c.ll.Len() >= c.cap {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.data, oldest.Value.(string))
		}
	}
	c.data[k] = c.ll.PushFront(k)
}

// --- Parameterized Hardened Ablation / GC-Isolation Engine (matches v2 protocol) ---

type evalEntry struct {
	method  string
	path    string
	handler liteLRU.HandlerFunc
	params  []liteLRU.Param
}

type evalChunk struct {
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

type paddedStripe struct {
	hits, misses, drops atomic.Int64
	_                   [liteLRU.CacheLineSize - 24]byte
}

type unpaddedStripe struct {
	hits, misses, drops atomic.Int64
}

func mmapSliceLocal[T any](n int) ([]T, []byte) {
	var zero T
	slab, err := memory.MmapAnonymous(int(unsafe.Sizeof(zero)) * n)
	if err != nil {
		panic(err)
	}
	return unsafe.Slice((*T)(unsafe.Pointer(unsafe.SliceData(slab))), n), slab
}

//go:nosplit
func evalWorkerStripe() uint {
	var anchor uintptr
	sp := uintptr(unsafe.Pointer(&anchor))
	return uint(((sp >> 11) * 0x9e3779b97f4a7c15) >> 57)
}

func pickFromHandLocal(cand uint64, hand uint32) uint32 {
	if above := cand & (^uint64(0) << (hand & 63)); above != 0 {
		return uint32(bits.TrailingZeros64(above))
	}
	return uint32(bits.TrailingZeros64(cand))
}

func promoteFreqLocal(chk *evalChunk, mask uint64) {
	if (chk.f0.Load() & mask) == 0 {
		chk.f0.Or(mask)
	} else if (chk.f1.Load() & mask) == 0 {
		chk.f1.Or(mask)
	}
}

type evalEngine struct {
	numGroups uint32
	useSWAR   bool
	usePad    bool
	entries   []atomic.Pointer[evalEntry]
	chunks    []evalChunk
	slab      []byte
	pStats    [128]paddedStripe
	uStats    [128]unpaddedStripe
}

func newEvalEngine(cap int, useSWAR, usePad, useMmap bool) *evalEngine {
	ng := uint32(cap / 64)
	var chunks []evalChunk
	var slab []byte
	if useMmap {
		chunks, slab = mmapSliceLocal[evalChunk](int(ng))
	} else {
		chunks = make([]evalChunk, ng)
	}
	return &evalEngine{
		numGroups: ng,
		useSWAR:   useSWAR,
		usePad:    usePad,
		entries:   make([]atomic.Pointer[evalEntry], cap),
		chunks:    chunks,
		slab:      slab,
	}
}

func (c *evalEngine) Close() {
	if c.slab != nil {
		memory.Munmap(c.slab)
		c.slab = nil
	}
}

func (c *evalEngine) incHit() {
	s := evalWorkerStripe()
	if c.usePad {
		c.pStats[s].hits.Add(1)
	} else {
		c.uStats[s].hits.Add(1)
	}
}

func (c *evalEngine) incMiss() {
	s := evalWorkerStripe()
	if c.usePad {
		c.pStats[s].misses.Add(1)
	} else {
		c.uStats[s].misses.Add(1)
	}
}

func (c *evalEngine) incDrop() {
	s := evalWorkerStripe()
	if c.usePad {
		c.pStats[s].drops.Add(1)
	} else {
		c.uStats[s].drops.Add(1)
	}
}

func (c *evalEngine) Get(method, path string) bool {
	h := liteLRU.HashRoute(method, path)
	group := uint32(h & uint64(c.numGroups-1))
	chk := &c.chunks[group]
	vb := chk.valid.Load()
	if vb == 0 {
		c.incMiss()
		return false
	}

	if !c.useSWAR {
		for m := vb; m != 0; {
			bit := uint32(bits.TrailingZeros64(m))
			m &= m - 1
			ep := c.entries[group*64+bit].Load()
			if ep != nil && ep.method == method && ep.path == path {
				promoteFreqLocal(chk, uint64(1)<<bit)
				c.incHit()
				return true
			}
		}
		c.incMiss()
		return false
	}

	sig8 := uint8(h >> 32)
	if sig8 == 0 {
		sig8 = 1
	}
	pat := uint64(sig8) * 0x0101010101010101
	for i := uint32(0); i < 8; i++ {
		vB := uint8(vb >> (i * 8))
		if vB == 0 {
			continue
		}
		word := chk.sigs[i].Load()
		v := word ^ pat
		matches := (v - 0x0101010101010101) &^ v & 0x8080808080808080
		for matches != 0 {
			tz := bits.TrailingZeros64(matches)
			matches &= matches - 1
			j := uint32(tz >> 3)
			if (vB&(1<<j)) == 0 || uint8(word>>(j*8)) != sig8 {
				continue
			}
			bit := i*8 + j
			ep := c.entries[group*64+bit].Load()
			if ep != nil && ep.method == method && ep.path == path {
				promoteFreqLocal(chk, uint64(1)<<bit)
				c.incHit()
				return true
			}
		}
	}
	c.incMiss()
	return false
}

func (c *evalEngine) findVictim(group uint32) uint32 {
	chk := &c.chunks[group]
	const smallTarget = 4
	for retries := 0; retries < 10; retries++ {
		vb := chk.valid.Load()
		wb := chk.writing.Load()
		avail := ^wb
		if emp := ^vb & avail; emp != 0 {
			bit := uint32(bits.TrailingZeros64(emp))
			if chk.writing.CompareAndSwap(wb, wb|(uint64(1)<<bit)) {
				return group*64 + bit
			}
			continue
		}
		prob := chk.prob.Load() & vb
		f0 := chk.f0.Load()
		if hs := prob & f0; hs != 0 {
			chk.prob.And(^hs)
			chk.f0.And(^hs)
			chk.f1.And(^hs)
			prob &= ^hs
		}
		if bits.OnesCount64(prob) >= smallTarget {
			if coldS := prob & ^f0 & avail; coldS != 0 {
				bit := pickFromHandLocal(coldS, chk.hand.Load())
				if chk.writing.CompareAndSwap(wb, wb|(uint64(1)<<bit)) {
					chk.hand.Store((bit + 1) & 63)
					return group*64 + bit
				}
				continue
			}
		}
		mainB := vb & ^prob
		if mainB != 0 {
			f0 = chk.f0.Load()
			if coldM := mainB & ^f0 & avail; coldM != 0 {
				bit := pickFromHandLocal(coldM, chk.handM.Load())
				if chk.writing.CompareAndSwap(wb, wb|(uint64(1)<<bit)) {
					chk.handM.Store((bit + 1) & 63)
					return group*64 + bit
				}
				continue
			}
			f1 := chk.f1.Load()
			chk.f0.And(^mainB | f1)
			chk.f1.And(^mainB)
		} else if cold := vb & avail; cold != 0 {
			chk.f0.Store(0)
			bit := pickFromHandLocal(cold, chk.hand.Load())
			if chk.writing.CompareAndSwap(wb, wb|(uint64(1)<<bit)) {
				chk.hand.Store((bit + 1) & 63)
				return group*64 + bit
			}
			continue
		}
		if wb != 0 {
			runtime.Gosched()
		}
	}
	return 0xFFFFFFFF
}

func (c *evalEngine) Add(method, path string) {
	h := liteLRU.HashRoute(method, path)
	group := uint32(h & uint64(c.numGroups-1))
	chk := &c.chunks[group]
	sig8 := uint8(h >> 32)
	if sig8 == 0 {
		sig8 = 1
	}
	ne := &evalEntry{method: method, path: path}

	if vb := chk.valid.Load(); vb != 0 {
		pat := uint64(sig8) * 0x0101010101010101
		tryUpdate := func(bit uint32) (done,retryOuter bool) {
			idx := group*64 + bit
			ep := c.entries[idx].Load()
			if ep == nil || ep.method != method || ep.path != path {
				return false, false
			}
			mask := uint64(1) << bit
			for r := 0; ; r++ {
				w := chk.writing.Load()
				if (w & mask) != 0 {
					c.incDrop()
					return true, false
				}
				if chk.writing.CompareAndSwap(w, w|mask) {
					break
				}
				if r+1 >= 10 {
					c.incDrop()
					return true, false
				}
			}
			cur := c.entries[idx].Load()
			if (chk.valid.Load()&mask) == 0 || cur == nil || cur.method != method || cur.path != path {
				chk.writing.And(^mask)
				return false, true
			}
			c.entries[idx].Store(ne)
			promoteFreqLocal(chk, mask)
			chk.writing.And(^mask)
			return true, false
		}

		if !c.useSWAR {
			for m := vb; m != 0; {
				bit := uint32(bits.TrailingZeros64(m))
				m &= m - 1
				if done, _ := tryUpdate(bit); done {
					return
				}
			}
		} else {
			for i := uint32(0); i < 8; i++ {
				vB := uint8(vb >> (i * 8))
				if vB == 0 {
					continue
				}
				word := chk.sigs[i].Load()
				v := word ^ pat
				matches := (v - 0x0101010101010101) &^ v & 0x8080808080808080
				for matches != 0 {
					tz := bits.TrailingZeros64(matches)
					matches &= matches - 1
					j := uint32(tz >> 3)
					if (vB&(1<<j)) == 0 || uint8(word>>(j*8)) != sig8 {
						continue
					}
					if done, _ := tryUpdate(i*8 + j); done {
						return
					}
				}
			}
		}
	}

	victim := c.findVictim(group)
	if victim == 0xFFFFFFFF {
		c.incDrop()
		return
	}
	bit := victim % 64
	mask := uint64(1) << bit
	chk.valid.And(^mask)
	if old := c.entries[victim].Load(); old != nil && (chk.prob.Load()&mask) != 0 {
		oh := liteLRU.HashRoute(old.method, old.path)
		b0 := uint64(1) << (uint(oh>>40) & 63)
		b1 := uint64(1) << (uint(oh>>48) & 63)
		if bits.OnesCount64(chk.ghost0.Load()) >= 34 {
			chk.ghost0.Store(b0)
			chk.ghost1.Store(b1)
		} else {
			chk.ghost0.Or(b0)
			chk.ghost1.Or(b1)
		}
	}
	c.entries[victim].Store(ne)
	if c.useSWAR {
		sw := bit / 8
		sh := (bit % 8) * 8
		for {
			ow := chk.sigs[sw].Load()
			nw := (ow & ^(uint64(0xFF) << sh)) | (uint64(sig8) << sh)
			if chk.sigs[sw].CompareAndSwap(ow, nw) {
				break
			}
		}
	}
	b0 := uint64(1) << (uint(h>>40) & 63)
	b1 := uint64(1) << (uint(h>>48) & 63)
	if (chk.ghost0.Load()&b0) != 0 && (chk.ghost1.Load()&b1) != 0 {
		chk.prob.And(^mask)
	} else {
		chk.prob.Or(mask)
	}
	chk.f0.And(^mask)
	chk.f1.And(^mask)
	chk.valid.Or(mask)
	chk.writing.And(^mask)
}

// --- Runners ---

func runWorkers(ops []string, workers int, getFn, addFn func(string)) (opsPerSec float64, p50, p99 time.Duration) {
	perW := len(ops) / workers
	samples := make([][]time.Duration, workers)
	for w := 0; w < workers; w++ {
		samples[w] = make([]time.Duration, 0, perW/32+1)
	}
	var wg sync.WaitGroup
	wg.Add(workers)
	start := time.Now()
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			s := id * perW
			e := s + perW
			local := samples[id]
			for i := s; i < e; i++ {
				if (i & 31) == 0 {
					t0 := time.Now()
					if i%5 == 0 {
						addFn(ops[i])
					} else {
						getFn(ops[i])
					}
					local = append(local, time.Since(t0))
				} else {
					if i%5 == 0 {
						addFn(ops[i])
					} else {
						getFn(ops[i])
					}
				}
			}
			samples[id] = local
		}(w)
	}
	wg.Wait()
	opsPerSec = float64(len(ops)) / time.Since(start).Seconds()
	var all []time.Duration
	for _, s := range samples {
		all = append(all, s...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	p50 = all[len(all)/2]
	p99 = all[int(float64(len(all))*0.99)]
	return
}

func runNoSampleMixed(ops []string, workers int, getEveryNum, getEveryDen int, getFn, addFn func(string)) float64 {
	perW := len(ops) / workers
	var wg sync.WaitGroup
	wg.Add(workers)
	start := time.Now()
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			s := id * perW
			e := s + perW
			for i := s; i < e; i++ {
				if (i % getEveryDen) < getEveryNum {
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

func runPureGet(ops []string, workers int, getFn func(string)) float64 {
	perW := len(ops) / workers
	var wg sync.WaitGroup
	wg.Add(workers)
	start := time.Now()
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			s := id * perW
			e := s + perW
			for i := s; i < e; i++ {
				getFn(ops[i])
			}
		}(w)
	}
	wg.Wait()
	return float64(len(ops)) / time.Since(start).Seconds()
}

func spinPenalty(d time.Duration) {
	if d <= 0 {
		return
	}
	t0 := time.Now()
	for time.Since(t0) < d {
	}
}

func runGCSub(mode string) {
	useMmap := mode[0] == '1'
	policy := mode[2:]

	if policy == "memlimit64" {
		debug.SetGCPercent(-1)
		debug.SetMemoryLimit(64 << 20)
	} else {
		debug.SetGCPercent(100)
	}

	var offHeap *liteLRU.LRUCache
	var onHeap *evalEngine
	if useMmap {
		offHeap = liteLRU.NewLRUCache(65536, 10)
		defer offHeap.Close()
	} else {
		onHeap = newEvalEngine(65536, true, true, false)
	}

	preKeys := make([]string, 65536)
	for i := 0; i < 65536; i++ {
		preKeys[i] = "/gc/" + strconv.Itoa(i)
		if useMmap {
			offHeap.Add("GET", preKeys[i], nil, nil)
		} else {
			onHeap.Add("GET", preKeys[i])
		}
	}

	runtime.GC()
	var mBefore, mMid, mLive runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	const window = 2 * time.Second
	var opsTotal atomic.Uint64
	var stop atomic.Bool
	var wg sync.WaitGroup

	if policy == "steady" {
		wg.Add(8)
		st := time.Now()
		for w := 0; w < 8; w++ {
			go func(id int) {
				defer wg.Done()
				var n uint64
				idx := id * 997
				for !stop.Load() {
					k := preKeys[idx&65535]
					if useMmap {
						offHeap.Get("GET", k, nil)
					} else {
						onHeap.Get("GET", k)
					}
					idx += 131
					n++
				}
				opsTotal.Add(n)
			}(w)
		}
		for g := 0; g < 20; g++ {
			time.Sleep(window / 20)
			runtime.GC()
		}
		stop.Store(true)
		wg.Wait()
		dur := time.Since(st)
		runtime.ReadMemStats(&mMid)
		runtime.GC()
		runtime.ReadMemStats(&mLive)
		runtime.KeepAlive(offHeap)
		runtime.KeepAlive(onHeap)
		runtime.KeepAlive(preKeys)
		numGC := mMid.NumGC - mBefore.NumGC
		pauseMs := float64(mMid.PauseTotalNs-mBefore.PauseTotalNs) / 1e6
		meanMs := 0.0
		if numGC > 0 {
			meanMs = pauseMs / float64(numGC)
		}
		fmt.Printf("%-26s | ops/s=%9.0f | NumGC=%3d | Pause=%.3fms (mean %.3fms) | HeapAlloc=%.2fMB | HeapInuse=%.2fMB | HeapObjLive=%7d | Mallocs=%8d | TotalAlloc=%.2fMB | GCCPU=%.5f\n",
			mode, float64(opsTotal.Load())/dur.Seconds(), numGC, pauseMs, meanMs,
			float64(mLive.HeapAlloc)/(1<<20), float64(mLive.HeapInuse)/(1<<20),
			mLive.HeapObjects, mMid.Mallocs-mBefore.Mallocs,
			float64(mMid.TotalAlloc-mBefore.TotalAlloc)/(1<<20), mMid.GCCPUFraction)
		return
	}

	wg.Add(8)
	st := time.Now()
	for w := 0; w < 8; w++ {
		go func(id int) {
			defer wg.Done()
			var sink []byte
			var n uint64
			for i := 0; !stop.Load(); i++ {
				k := preKeys[(id*997+i)&65535]
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
				n++
			}
			runtime.KeepAlive(sink)
			opsTotal.Add(n)
		}(w)
	}
	time.Sleep(window)
	stop.Store(true)
	wg.Wait()
	dur := time.Since(st)
	runtime.ReadMemStats(&mMid)
	runtime.GC()
	runtime.ReadMemStats(&mLive)
	runtime.KeepAlive(offHeap)
	runtime.KeepAlive(onHeap)
	runtime.KeepAlive(preKeys)
	numGC := mMid.NumGC - mBefore.NumGC
	pauseMs := float64(mMid.PauseTotalNs-mBefore.PauseTotalNs) / 1e6
	meanMs := 0.0
	if numGC > 0 {
		meanMs = pauseMs / float64(numGC)
	}
	fmt.Printf("%-26s | ops/s=%9.0f | NumGC=%3d | Pause=%.3fms (mean %.3fms) | HeapObjMid=%7d | HeapObjLive=%7d | LiveAlloc=%.2fMB | Inuse=%.2fMB | Mallocs=%8d | Frees=%8d | TotalAlloc=%.2fMB | GCCPU=%.5f\n",
		mode, float64(opsTotal.Load())/dur.Seconds(), numGC, pauseMs, meanMs,
		mMid.HeapObjects, mLive.HeapObjects,
		float64(mLive.HeapAlloc)/(1<<20), float64(mLive.HeapInuse)/(1<<20),
		mMid.Mallocs-mBefore.Mallocs, mMid.Frees-mBefore.Frees,
		float64(mMid.TotalAlloc-mBefore.TotalAlloc)/(1<<20), mMid.GCCPUFraction)
}

func main() {
	onlyBaseline := flag.Bool("only-baseline", false, "run only section 1 baseline")
	onlyAblation := flag.Bool("only-ablation", false, "run only section 2 hardened ablation")
	onlyScale := flag.Bool("only-scale", false, "run only section 3 zipfian scaling")
	onlyWrite := flag.Bool("only-write", false, "run only section 4 write-heavy + semantic-equivalence controls")
	onlyZipf := flag.Bool("only-zipf", false, "run only section 5 matched-capacity zipf hit rate")
	onlyGC := flag.Bool("only-gc", false, "run only section 6 isolated GC suite")
	gcSub := flag.String("gc-sub", "", "internal subprocess GC benchmark mode")
	flag.Parse()

	if *gcSub != "" {
		runGCSub(*gcSub)
		return
	}

	const runs = 10
	const numKeys = 1000
	const numOps = 1600000
	const capacity = 1024

	rng := rand.New(rand.NewSource(42))
	keys := make([]string, numKeys)
	for i := 0; i < numKeys; i++ {
		keys[i] = fmt.Sprintf("GET:/api/v1/resource/%d", i)
	}
	uniformOps := make([]string, numOps)
	for i := 0; i < numOps; i++ {
		if rng.Float64() < 0.7 {
			uniformOps[i] = keys[rng.Intn(200)]
		} else {
			uniformOps[i] = keys[rng.Intn(numKeys)]
		}
	}

	onlyAll := !*onlyBaseline && !*onlyAblation && !*onlyScale && !*onlyWrite && !*onlyZipf && !*onlyGC

	if onlyAll || *onlyBaseline {
		fmt.Println("=== 1. BASELINE (10 runs, median + IQR) ===")
		var lOps, oOps, mOps []float64
		var lp50, lp99, op50, op99, mp50, mp99 []time.Duration
		for r := 0; r < runs; r++ {
			lc := liteLRU.NewLRUCache(capacity, 10)
			ops, p50, p99 := runWorkers(uniformOps, 8, func(k string) { lc.Get("GET", k, nil) }, func(k string) { lc.Add("GET", k, nil, nil) })
			lc.Close()
			lOps, lp50, lp99 = append(lOps, ops), append(lp50, p50), append(lp99, p99)

			oc, _ := otter.MustBuilder[string, any](capacity).Build()
			ops, p50, p99 = runWorkers(uniformOps, 8, func(k string) { oc.Get(k) }, func(k string) { oc.Set(k, 1) })
			oc.Close()
			oOps, op50, op99 = append(oOps, ops), append(op50, p50), append(op99, p99)

			mc := newMutexLRU(capacity)
			ops, p50, p99 = runWorkers(uniformOps, 8, func(k string) { mc.Get(k) }, func(k string) { mc.Add(k, 1) })
			mOps, mp50, mp99 = append(mOps, ops), append(mp50, p50), append(mp99, p99)
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

	if onlyAll || *onlyAblation {
		var bucketCounts [16]int
		for _, k := range keys {
			bucketCounts[liteLRU.HashRoute("GET", k)%16]++
		}
		minH, maxH, overChunks, overKeys := 1000, 0, 0, 0
		for _, cnt := range bucketCounts {
			if cnt < minH {
				minH = cnt
			}
			if cnt > maxH {
				maxH = cnt
			}
			if cnt > 64 {
				overChunks++
				overKeys += cnt - 64
			}
		}
		diagCache := liteLRU.NewLRUCache(capacity, 10)
		runNoSampleMixed(uniformOps, 8, 4, 5, func(k string) { diagCache.Get("GET", k, nil) }, func(k string) { diagCache.Add("GET", k, nil, nil) })
		hitsA, missesA, dropsA, ratioA := diagCache.Stats()
		diagCache.Close()

		const perChunkResident = 60
		var perChunkKeys [16][]string
		var residentKeys []string
		for idx := 0; len(residentKeys) < 16*perChunkResident; idx++ {
			k := fmt.Sprintf("GET:/resident/%d", idx)
			g := liteLRU.HashRoute("GET", k) % 16
			if len(perChunkKeys[g]) < perChunkResident {
				perChunkKeys[g] = append(perChunkKeys[g], k)
				residentKeys = append(residentKeys, k)
			}
		}
		resOps := make([]string, numOps)
		for i := 0; i < numOps; i++ {
			resOps[i] = residentKeys[i%len(residentKeys)]
		}

		runAblation := func(label string, ops []string, prewarm []string, pureGet bool, getNum, getDen int) {
			fmt.Printf("\n--- %s ---\n", label)
			var aFull, aNoSWAR, aNoPad, aMutexLRU, aRWMap []float64
			for r := 0; r < runs; r++ {
				fc := liteLRU.NewLRUCache(capacity, 10)
				for _, pk := range prewarm {
					fc.Add("GET", pk, nil, nil)
				}
				if pureGet {
					aFull = append(aFull, runPureGet(ops, 8, func(k string) { fc.Get("GET", k, nil) }))
				} else {
					aFull = append(aFull, runNoSampleMixed(ops, 8, getNum, getDen, func(k string) { fc.Get("GET", k, nil) }, func(k string) { fc.Add("GET", k, nil, nil) }))
				}
				fc.Close()

				ns := newEvalEngine(capacity, false, true, true)
				for _, pk := range prewarm {
					ns.Add("GET", pk)
				}
				if pureGet {
					aNoSWAR = append(aNoSWAR, runPureGet(ops, 8, func(k string) { ns.Get("GET", k) }))
				} else {
					aNoSWAR = append(aNoSWAR, runNoSampleMixed(ops, 8, getNum, getDen, func(k string) { ns.Get("GET", k) }, func(k string) { ns.Add("GET", k) }))
				}
				ns.Close()

				np := newEvalEngine(capacity, true, false, true)
				for _, pk := range prewarm {
					np.Add("GET", pk)
				}
				if pureGet {
					aNoPad = append(aNoPad, runPureGet(ops, 8, func(k string) { np.Get("GET", k) }))
				} else {
					aNoPad = append(aNoPad, runNoSampleMixed(ops, 8, getNum, getDen, func(k string) { np.Get("GET", k) }, func(k string) { np.Add("GET", k) }))
				}
				np.Close()

				mc := newMutexLRU(capacity)
				for _, pk := range prewarm {
					mc.Add(pk, 1)
				}
				if pureGet {
					aMutexLRU = append(aMutexLRU, runPureGet(ops, 8, func(k string) { mc.Get(k) }))
				} else {
					aMutexLRU = append(aMutexLRU, runNoSampleMixed(ops, 8, getNum, getDen, func(k string) { mc.Get(k) }, func(k string) { mc.Add(k, 1) }))
				}

				rw := newRWMapCache(capacity)
				for _, pk := range prewarm {
					rw.Add(pk)
				}
				if pureGet {
					aRWMap = append(aRWMap, runPureGet(ops, 8, func(k string) { rw.Get(k) }))
				} else {
					aRWMap = append(aRWMap, runNoSampleMixed(ops, 8, getNum, getDen, func(k string) { rw.Get(k) }, func(k string) { rw.Add(k) }))
				}
			}
			fm, fq1, fq3 := medianAndIQR(aFull)
			nsm, nsq1, nsq3 := medianAndIQR(aNoSWAR)
			npm, npq1, npq3 := medianAndIQR(aNoPad)
			mlm, mlq1, mlq3 := medianAndIQR(aMutexLRU)
			rwm, rwq1, rwq3 := medianAndIQR(aRWMap)
			fmt.Printf("Full v2 (mmap): med=%.0f (IQR %.0f-%.0f)\n", fm, fq1, fq3)
			fmt.Printf("No SWAR (mmap): med=%.0f (IQR %.0f-%.0f) [%.1f%%]\n", nsm, nsq1, nsq3, (nsm-fm)/fm*100)
			fmt.Printf("No Pad  (mmap): med=%.0f (IQR %.0f-%.0f) [%.1f%%]\n", npm, npq1, npq3, (npm-fm)/fm*100)
			fmt.Printf("True MutexLRU:  med=%.0f (IQR %.0f-%.0f) [%.1f%%]\n", mlm, mlq1, mlq3, (mlm-fm)/fm*100)
			fmt.Printf("Map+RWMutex*:   med=%.0f (IQR %.0f-%.0f) [%.1f%%]\n", rwm, rwq1, rwq3, (rwm-fm)/fm*100)
		}

		fmt.Println("\n=== 2. HARDENED ABLATION (10 runs, median + IQR, identical mmap placement across Full/NoSWAR/NoPad) ===")
		fmt.Printf("Regime A diagnostics (1000 keys in 16 sets): minH=%d maxH=%d overChunks=%d/16 overflowKeys=%d | hits=%d misses=%d (hitRate=%.2f%%) drops=%d\n",
			minH, maxH, overChunks, overKeys, hitsA, missesA, ratioA*100, dropsA)
		runAblation("Regime A0: Zero-Overflow Pre-Warmed 100% Get (960 keys, <=60/chunk, 0 misses/evictions)", resOps, residentKeys, true, 1, 1)
		runAblation("Regime A1: 80/20 Uniform-Pool (1000 keys, cap=1024)", uniformOps, nil, false, 4, 5)
		runAblation("Regime B: 80/20 Zipfian (100k keys, cap=1024, active eviction)", zipfOps, nil, false, 4, 5)
		runAblation("Regime C: 50/50 Zipfian (100k keys, cap=1024, write-heavy)", zipfOps, nil, false, 1, 2)
	}

	if *onlyAblation {
		return
	}

	if onlyAll || *onlyScale {
		fmt.Println("\n=== 3. ZIPFIAN SCALING (10 runs, s=1.001, N=100k, cap=1024) ===")
		var base1Core float64
		for _, cores := range []int{1, 2, 4, 8} {
			var vals []float64
			for r := 0; r < runs; r++ {
				c := liteLRU.NewLRUCache(capacity, 10)
				vals = append(vals, runNoSampleMixed(zipfOps, cores, 4, 5, func(k string) { c.Get("GET", k, nil) }, func(k string) { c.Add("GET", k, nil, nil) }))
				c.Close()
			}
			m, q1, q3 := medianAndIQR(vals)
			if cores == 1 {
				base1Core = m
			}
			fmt.Printf("%d cores: med=%.0f (IQR %.0f-%.0f) [%.2fx]\n", cores, m, q1, q3, m/base1Core)
		}
	}

	if onlyAll || *onlyWrite {
		fmt.Println("\n=== 4. WRITE-HEAVY ZIPF, SEMANTIC-EQUIVALENCE, & MISS-PENALTY CONTROLS (10 runs) ===")
		fmt.Println("--- 4a. Direct Overwrite / Value-Update Contention (N=1000, cap=1024) ---")
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

		fmt.Println("--- 4b. Immutable Insert-on-Miss Working-Set Maintenance (including large-cap 16384 vs Otter buffered size) ---")
		for _, spec := range []struct {
			capSize int
			wsSize  int
		}{
			{1024, 1000},
			{1024, 5000},
			{16384, 100000},
		} {
			zr := rand.New(rand.NewSource(77))
			z := rand.NewZipf(zr, 1.001, 1, uint64(spec.wsSize-1))
			ks := make([]string, spec.wsSize)
			for i := range ks {
				ks[i] = strconv.Itoa(i)
			}
			mOps := make([]string, numOps)
			for i := range mOps {
				mOps[i] = ks[z.Uint64()]
			}
			var lOpsM, lHitM, lDropAbsM, oOpsM, oHitM, oLiveSz []float64
			for r := 0; r < runs; r++ {
				lc := liteLRU.NewLRUCache(spec.capSize, 10)
				var wg sync.WaitGroup
				wg.Add(8)
				chunk := numOps / 8
				st := time.Now()
				for w := 0; w < 8; w++ {
					go func(s, e int) {
						defer wg.Done()
						for i := s; i < e; i++ {
							k := mOps[i]
							if _, _, ok := lc.Get("GET", k, nil); !ok {
								lc.Add("GET", k, nil, nil)
							}
						}
					}(w*chunk, (w+1)*chunk)
				}
				wg.Wait()
				dur := time.Since(st).Seconds()
				_, misses, drops, ratio := lc.Stats()
				lc.Close()
				lOpsM = append(lOpsM, float64(numOps)/dur)
				lHitM = append(lHitM, ratio*100)
				dropPctMiss := 0.0
				if misses > 0 {
					dropPctMiss = float64(drops) / float64(misses) * 100
				}
				lDropAbsM = append(lDropAbsM, dropPctMiss)

				oc, _ := otter.MustBuilder[string, any](spec.capSize).Build()
				var oHits atomic.Uint64
				wg.Add(8)
				st = time.Now()
				for w := 0; w < 8; w++ {
					go func(s, e int) {
						defer wg.Done()
						var localHits uint64
						for i := s; i < e; i++ {
							k := mOps[i]
							if _, ok := oc.Get(k); ok {
								localHits++
							} else {
								oc.Set(k, nil)
							}
						}
						oHits.Add(localHits)
					}(w*chunk, (w+1)*chunk)
				}
				wg.Wait()
				dur = time.Since(st).Seconds()
				liveSz := float64(oc.Size())
				oc.Close()
				oOpsM = append(oOpsM, float64(numOps)/dur)
				oHitM = append(oHitM, float64(oHits.Load())/float64(numOps)*100)
				oLiveSz = append(oLiveSz, liveSz)
			}
			lm, lq1, lq3 := medianAndIQR(lOpsM)
			lhm, _, _ := medianAndIQR(lHitM)
			ldm, _, _ := medianAndIQR(lDropAbsM)
			om, oq1, oq3 := medianAndIQR(oOpsM)
			ohm, _, _ := medianAndIQR(oHitM)
			osz, _, _ := medianAndIQR(oLiveSz)
			fmt.Printf("Insert-on-Miss cap=%d N=%d: liteLRU=%.0f (%.0f-%.0f) hit=%.2f%% missDrop=%.2f%% | otter=%.0f (%.0f-%.0f) hit=%.2f%% liveSize=%.0f | adv=%.2fx\n",
				spec.capSize, spec.wsSize, lm, lq1, lq3, lhm, ldm, om, oq1, oq3, ohm, osz, lm/om)
		}

		fmt.Println("--- 4c. Backend Miss-Penalty Control (T_miss = 5us on miss, cap=16384, N=100k, 400k ops, 10 runs) ---")
		{
			const pOpsN = 400000
			zr := rand.New(rand.NewSource(77))
			z := rand.NewZipf(zr, 1.001, 1, 99999)
			ks := make([]string, 100000)
			for i := range ks {
				ks[i] = strconv.Itoa(i)
			}
			pOps := make([]string, pOpsN)
			for i := range pOps {
				pOps[i] = ks[z.Uint64()]
			}
			var lPenOps, oPenOps, lPenHit, oPenHit []float64
			for r := 0; r < runs; r++ {
				lc := liteLRU.NewLRUCache(16384, 10)
				var wg sync.WaitGroup
				wg.Add(8)
				chunk := pOpsN / 8
				st := time.Now()
				for w := 0; w < 8; w++ {
					go func(s, e int) {
						defer wg.Done()
						for i := s; i < e; i++ {
							k := pOps[i]
							if _, _, ok := lc.Get("GET", k, nil); !ok {
								spinPenalty(5 * time.Microsecond)
								lc.Add("GET", k, nil, nil)
							}
						}
					}(w*chunk, (w+1)*chunk)
				}
				wg.Wait()
				dur := time.Since(st).Seconds()
				_, _, _, ratio := lc.Stats()
				lc.Close()
				lPenOps = append(lPenOps, float64(pOpsN)/dur)
				lPenHit = append(lPenHit, ratio*100)

				oc, _ := otter.MustBuilder[string, any](16384).Build()
				var oHits atomic.Uint64
				wg.Add(8)
				st = time.Now()
				for w := 0; w < 8; w++ {
					go func(s, e int) {
						defer wg.Done()
						var lh uint64
						for i := s; i < e; i++ {
							k := pOps[i]
							if _, ok := oc.Get(k); ok {
								lh++
							} else {
								spinPenalty(5 * time.Microsecond)
								oc.Set(k, nil)
							}
						}
						oHits.Add(lh)
					}(w*chunk, (w+1)*chunk)
				}
				wg.Wait()
				dur = time.Since(st).Seconds()
				oc.Close()
				oPenOps = append(oPenOps, float64(pOpsN)/dur)
				oPenHit = append(oPenHit, float64(oHits.Load())/float64(pOpsN)*100)
			}
			lm, lq1, lq3 := medianAndIQR(lPenOps)
			lhm, _, _ := medianAndIQR(lPenHit)
			om, oq1, oq3 := medianAndIQR(oPenOps)
			ohm, _, _ := medianAndIQR(oPenHit)
			fmt.Printf("MissPenalty(5us) cap=16384 N=100k: liteLRU=%.0f (%.0f-%.0f) hit=%.2f%% | otter=%.0f (%.0f-%.0f) hit=%.2f%% | adv=%.2fx\n",
				lm, lq1, lq3, lhm, om, oq1, oq3, ohm, lm/om)
		}
	}

	if onlyAll || *onlyZipf {
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
	}

	if onlyAll || *onlyGC {
		fmt.Println("\n=== 6. GC ISOLATION SUITE (65,536 slots, isolated subprocesses, identical v2 entry/chunk layout) ===")
		exe, err := os.Executable()
		if err != nil {
			panic(err)
		}
		for _, mode := range []string{"1:gogc100", "0:gogc100", "1:memlimit64", "0:memlimit64", "1:steady", "0:steady"} {
			cmd := exec.Command(exe, "--gc-sub", mode)
			out, err := cmd.CombinedOutput()
			if err != nil {
				fmt.Printf("mode %s error: %v (%s)\n", mode, err, string(out))
				continue
			}
			fmt.Print(string(out))
		}
	}
}
