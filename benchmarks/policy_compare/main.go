package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
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
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}

func genZipfOps(seed int64, s float64, nKeys, totalOps int) []string {
	rng := rand.New(rand.NewSource(seed))
	if s > 1.0001 {
		z := rand.NewZipf(rng, s, 1, uint64(nKeys-1))
		ops := make([]string, totalOps)
		for i := 0; i < totalOps; i++ {
			ops[i] = strconv.FormatUint(z.Uint64(), 10)
		}
		return ops
	}
	pmf := make([]float64, nKeys)
	var sum float64
	for i := 1; i <= nKeys; i++ {
		p := 1.0 / math.Pow(float64(i), s)
		pmf[i-1] = p
		sum += p
	}
	cdf := make([]float64, nKeys)
	var csum float64
	for i := 0; i < nKeys; i++ {
		csum += pmf[i] / sum
		cdf[i] = csum
	}
	ops := make([]string, totalOps)
	for i := 0; i < totalOps; i++ {
		rv := rng.Float64()
		idx := sort.SearchFloat64s(cdf, rv)
		if idx >= nKeys {
			idx = nKeys - 1
		}
		ops[i] = strconv.Itoa(idx)
	}
	return ops
}

type workloadStats struct {
	opsPerSec  float64
	p50        time.Duration
	p99        time.Duration
	p999       time.Duration
	hitRate    float64
	dropPct    float64
	effOpsSec  float64
	addsTotal  int64
	dropsTotal int64
}

func runConcurrent(ops []string, workers int, getPct int, fnGet func(string) bool, fnAdd func(string)) (float64, time.Duration, time.Duration, time.Duration, int64) {
	n := len(ops)
	chunk := n / workers
	var wg sync.WaitGroup
	wg.Add(workers)

	var adds atomic.Int64
	var latMu sync.Mutex
	var allSamples []time.Duration

	start := time.Now()
	for w := 0; w < workers; w++ {
		sIdx := w * chunk
		eIdx := sIdx + chunk
		if w == workers-1 {
			eIdx = n
		}
		go func(startI, endI int) {
			defer wg.Done()
			local := make([]time.Duration, 0, (endI-startI)/64+16)
			for i := startI; i < endI; i++ {
				sample := (i & 63) == 0
				var t0 time.Time
				if sample {
					t0 = time.Now()
				}
				if (i % 100) < getPct {
					fnGet(ops[i])
				} else {
					fnAdd(ops[i])
					adds.Add(1)
				}
				if sample {
					local = append(local, time.Since(t0))
				}
			}
			latMu.Lock()
			allSamples = append(allSamples, local...)
			latMu.Unlock()
		}(sIdx, eIdx)
	}
	wg.Wait()
	dur := time.Since(start)

	sort.Slice(allSamples, func(i, j int) bool { return allSamples[i] < allSamples[j] })
	var p50, p99, p999 time.Duration
	if m := len(allSamples); m > 0 {
		p50 = allSamples[m/2]
		p99 = allSamples[(m*99)/100]
		p999 = allSamples[(m*999)/1000]
	}
	return float64(n) / dur.Seconds(), p50, p99, p999, adds.Load()
}

func runInsertOnMiss(ops []string, workers int, fnGet func(string) bool, fnAdd func(string)) (float64, time.Duration, time.Duration, time.Duration, int64, int64) {
	n := len(ops)
	chunk := n / workers
	var wg sync.WaitGroup
	wg.Add(workers)

	var hits, misses atomic.Int64
	var latMu sync.Mutex
	var allSamples []time.Duration

	start := time.Now()
	for w := 0; w < workers; w++ {
		sIdx := w * chunk
		eIdx := sIdx + chunk
		if w == workers-1 {
			eIdx = n
		}
		go func(startI, endI int) {
			defer wg.Done()
			local := make([]time.Duration, 0, (endI-startI)/64+16)
			var lh, lm int64
			for i := startI; i < endI; i++ {
				sample := (i & 63) == 0
				var t0 time.Time
				if sample {
					t0 = time.Now()
				}
				k := ops[i]
				if fnGet(k) {
					lh++
				} else {
					lm++
					fnAdd(k)
				}
				if sample {
					local = append(local, time.Since(t0))
				}
			}
			hits.Add(lh)
			misses.Add(lm)
			latMu.Lock()
			allSamples = append(allSamples, local...)
			latMu.Unlock()
		}(sIdx, eIdx)
	}
	wg.Wait()
	dur := time.Since(start)

	sort.Slice(allSamples, func(i, j int) bool { return allSamples[i] < allSamples[j] })
	var p50, p99, p999 time.Duration
	if m := len(allSamples); m > 0 {
		p50 = allSamples[m/2]
		p99 = allSamples[(m*99)/100]
		p999 = allSamples[(m*999)/1000]
	}
	return float64(n) / dur.Seconds(), p50, p99, p999, hits.Load(), misses.Load()
}

func benchZipfHitRates(policies []liteLRU.EvictionPolicy) {
	fmt.Println("=== 1. MATCHED-CAPACITY ZIPFIAN HIT RATES (10 runs, N=100k, 400k warm + 1.6M 8-worker eval) ===")
	skews := []float64{1.001, 0.800}
	caps := []int{16384, 32768, 65536}
	const numKeys = 100000
	const warmup = 400000
	const evalOps = 1600000
	const totalOps = warmup + evalOps
	const runs = 10

	for _, s := range skews {
		fmt.Printf("\nSkew s=%.3f:\n", s)
		fmt.Printf("%-10s | %-10s | %-10s | %-10s | %-10s\n", "Cap", "CLOCK", "CLOCK+", "S3-FIFO", "Otter")
		pmf := make([]float64, numKeys)
		var sum float64
		for i := 1; i <= numKeys; i++ {
			p := 1.0 / math.Pow(float64(i), s)
			pmf[i-1] = p
			sum += p
		}
		cdf := make([]float64, numKeys)
		var csum float64
		for i := 0; i < numKeys; i++ {
			csum += pmf[i] / sum
			cdf[i] = csum
		}
		rng := rand.New(rand.NewSource(77))
		seq := make([]string, totalOps)
		for i := 0; i < totalOps; i++ {
			idx := sort.SearchFloat64s(cdf, rng.Float64())
			if idx >= numKeys {
				idx = numKeys - 1
			}
			seq[i] = strconv.Itoa(idx)
		}

		for _, capSize := range caps {
			res := make(map[string][]float64)
			for r := 0; r < runs; r++ {
				for _, pol := range policies {
					c := liteLRU.NewLRUCacheWithPolicy(capSize, 4, pol)
					for i := 0; i < warmup; i++ {
						if _, _, ok := c.Get("GET", seq[i], nil); !ok {
							c.Add("GET", seq[i], nil, nil)
						}
					}
					var hits atomic.Int64
					var wg sync.WaitGroup
					wg.Add(8)
					cs := evalOps / 8
					for w := 0; w < 8; w++ {
						go func(st, en int) {
							defer wg.Done()
							var lh int64
							for i := st; i < en; i++ {
								if _, _, ok := c.Get("GET", seq[i], nil); ok {
									lh++
								} else {
									c.Add("GET", seq[i], nil, nil)
								}
							}
							hits.Add(lh)
						}(warmup+w*cs, warmup+(w+1)*cs)
					}
					wg.Wait()
					res[pol.String()] = append(res[pol.String()], float64(hits.Load())/float64(evalOps)*100)
					c.Close()
				}
				oc, _ := otter.MustBuilder[string, int](capSize).Build()
				for i := 0; i < warmup; i++ {
					if _, ok := oc.Get(seq[i]); !ok {
						oc.Set(seq[i], 1)
					}
				}
				var ohits atomic.Int64
				var owg sync.WaitGroup
				owg.Add(8)
				cs := evalOps / 8
				for w := 0; w < 8; w++ {
					go func(st, en int) {
						defer owg.Done()
						var lh int64
						for i := st; i < en; i++ {
							if _, ok := oc.Get(seq[i]); ok {
								lh++
							} else {
								oc.Set(seq[i], 1)
							}
						}
						ohits.Add(lh)
					}(warmup+w*cs, warmup+(w+1)*cs)
				}
				owg.Wait()
				res["otter"] = append(res["otter"], float64(ohits.Load())/float64(evalOps)*100)
				oc.Close()
			}
			m1, _, _ := medianAndIQR(res["clock"])
			m2, _, _ := medianAndIQR(res["clock+"])
			m3, _, _ := medianAndIQR(res["s3fifo"])
			m4, _, _ := medianAndIQR(res["otter"])
			fmt.Printf("%-10d | %8.2f%% | %8.2f%% | %8.2f%% | %8.2f%%\n", capSize, m1, m2, m3, m4)
		}
	}
}

func benchConcurrentMatrix(policies []liteLRU.EvictionPolicy) {
	fmt.Println("\n=== 2. CONCURRENT THROUGHPUT, LATENCY, HIT RATE & DROPS (10 runs, 8 workers, 1.6M ops) ===")
	const runs = 10
	const workers = 8
	const totalOps = 1600000

	type scenario struct {
		name      string
		capSize   int
		nKeys     int
		getPct    int
		insOnMiss bool
	}
	scenarios := []scenario{
		{"Insert-on-Miss (cap=1k, N=1k)", 1024, 1000, 100, true},
		{"Insert-on-Miss (cap=1k, N=5k)", 1024, 5000, 100, true},
		{"Insert-on-Miss (cap=16k, N=100k)", 16384, 100000, 100, true},
		{"Zipf 80/20 Evict (cap=1k, N=100k)", 1024, 100000, 80, false},
		{"Direct Write 50/50 (cap=1k, N=1k)", 1024, 1000, 50, false},
		{"Direct Write 20/80 (cap=1k, N=1k)", 1024, 1000, 20, false},
	}

	zipf1k := genZipfOps(101, 1.001, 1000, totalOps)
	zipf5k := genZipfOps(102, 1.001, 5000, totalOps)
	zipf100k := genZipfOps(103, 1.001, 100000, totalOps)

	getSeq := func(nKeys int) []string {
		switch nKeys {
		case 1000:
			return zipf1k
		case 5000:
			return zipf5k
		default:
			return zipf100k
		}
	}

	for _, sc := range scenarios {
		fmt.Printf("\n[%s]\n", sc.name)
		fmt.Printf("%-8s | %-15s | %-9s | %-9s | %-9s | %-7s | %-9s\n",
			"Policy", "Med Ops/s (Eff)", "p50", "p99", "p99.9", "Hit%", "Drops%")
		seq := getSeq(sc.nKeys)
		for _, pol := range policies {
			var attOps, effOps, hitRates, dropPcts []float64
			var p50s, p99s, p999s []time.Duration
			for r := 0; r < runs; r++ {
				c := liteLRU.NewLRUCacheWithPolicy(sc.capSize, 4, pol)
				if sc.insOnMiss {
					ops, p50, p99, p999, h, m := runInsertOnMiss(seq, workers,
						func(k string) bool { _, _, ok := c.Get("GET", k, nil); return ok },
						func(k string) { c.Add("GET", k, nil, nil) })
					_, _, drops, _ := c.Stats()
					c.Close()
					attOps = append(attOps, ops)
					effOps = append(effOps, ops)
					hitRates = append(hitRates, float64(h)/float64(h+m)*100)
					dropPcts = append(dropPcts, float64(drops)/float64(m+1)*100)
					p50s, p99s, p999s = append(p50s, p50), append(p99s, p99), append(p999s, p999)
				} else {
					ops, p50, p99, p999, adds := runConcurrent(seq, workers, sc.getPct,
						func(k string) bool { _, _, ok := c.Get("GET", k, nil); return ok },
						func(k string) { c.Add("GET", k, nil, nil) })
					_, _, drops, ratio := c.Stats()
					c.Close()
					eff := ops * float64(int64(totalOps)-drops) / float64(totalOps)
					attOps = append(attOps, ops)
					effOps = append(effOps, eff)
					hitRates = append(hitRates, ratio*100)
					dropPcts = append(dropPcts, float64(drops)/float64(adds)*100)
					p50s, p99s, p999s = append(p50s, p50), append(p99s, p99), append(p999s, p999)
				}
			}
			mAtt, _, _ := medianAndIQR(attOps)
			mEff, _, _ := medianAndIQR(effOps)
			mHit, _, _ := medianAndIQR(hitRates)
			mDrop, _, _ := medianAndIQR(dropPcts)
			fmt.Printf("%-8s | %7.2fM(%6.2fM) | %-9v | %-9v | %-9v | %6.2f%% | %7.2f%%\n",
				pol, mAtt/1e6, mEff/1e6, medianDur(p50s), medianDur(p99s), medianDur(p999s), mHit, mDrop)
		}
	}
}

func benchAdversarial(policies []liteLRU.EvictionPolicy) {
	fmt.Println("\n=== 3. ADVERSARIAL & ASSOCIATIVITY BOUNDARY (cap=1024, 8 workers) ===")

	// 3a. 64 vs 65 keys in chunk 0, 1000 cyclic passes
	fmt.Printf("\n[Associativity Boundary: 64 vs 65 keys -> chunk 0, x1000]\n")
	fmt.Printf("%-8s | %-10s | %-10s\n", "Policy", "64 keys", "65 keys")
	for _, pol := range policies {
		var h64, h65 float64
		for _, nKeys := range []int{64, 65} {
			c := liteLRU.NewLRUCacheWithPolicy(1024, 4, pol)
			numGroups := uint64(1024 / 64)
			var keys []string
			for i := 0; len(keys) < nKeys; i++ {
				p := fmt.Sprintf("/bound/%d", i)
				if liteLRU.HashRoute("GET", p)%numGroups == 0 {
					keys = append(keys, p)
				}
			}
			for _, k := range keys {
				c.Add("GET", k, nil, nil)
			}
			hits := 0
			for r := 0; r < 1000; r++ {
				for _, k := range keys {
					if _, _, ok := c.Get("GET", k, nil); ok {
						hits++
					} else {
						c.Add("GET", k, nil, nil)
					}
				}
			}
			pct := float64(hits) / float64(1000*nKeys) * 100
			if nKeys == 64 {
				h64 = pct
			} else {
				h65 = pct
			}
			c.Close()
		}
		fmt.Printf("%-8s | %8.2f%% | %8.2f%%\n", pol, h64, h65)
	}

	// 3b. Scan resistance (256 hot + 2048 scan), both 1-Get warm and 2-Get warm
	fmt.Printf("\n[Sequential Scan Resistance: 256 hot + 2048 scan keys]\n")
	fmt.Printf("%-8s | %-16s | %-16s\n", "Policy", "1-Get warm", "2-Get warm")
	for _, pol := range policies {
		var ret [2]int
		for warmGets := 1; warmGets <= 2; warmGets++ {
			c := liteLRU.NewLRUCacheWithPolicy(1024, 4, pol)
			for i := 0; i < 256; i++ {
				k := fmt.Sprintf("/hot/%d", i)
				c.Add("GET", k, nil, nil)
				for g := 0; g < warmGets; g++ {
					c.Get("GET", k, nil)
				}
			}
			for i := 0; i < 2048; i++ {
				c.Add("GET", fmt.Sprintf("/scan/%d", i), nil, nil)
			}
			kept := 0
			for i := 0; i < 256; i++ {
				if _, _, ok := c.Get("GET", fmt.Sprintf("/hot/%d", i), nil); ok {
					kept++
				}
			}
			ret[warmGets-1] = kept
			c.Close()
		}
		fmt.Printf("%-8s | %3d/256 (%6.2f%%) | %3d/256 (%6.2f%%)\n",
			pol, ret[0], float64(ret[0])/256*100, ret[1], float64(ret[1])/256*100)
	}

	// 3c. Single-set 128-key contention + Single-key concurrent upsert
	numGroups := uint64(1024 / 64)
	chunk0Keys := make([]string, 0, 128)
	for i := 0; len(chunk0Keys) < 128; i++ {
		p := fmt.Sprintf("/chunk0/%d", i)
		if liteLRU.HashRoute("GET", p)%numGroups == 0 {
			chunk0Keys = append(chunk0Keys, p)
		}
	}
	fmt.Printf("\n[Chunk-0 128-key Contention (80/20) & Single-Key Upsert (8 workers, 1.6M ops)]\n")
	fmt.Printf("%-8s | %-22s | %-22s\n", "Policy", "128-key Ch0 (Att/Eff/Drop/Hit)", "1-Key Upsert (Att/Eff/Drop%)")
	for _, pol := range policies {
		// 128-key chunk 0
		c := liteLRU.NewLRUCacheWithPolicy(1024, 4, pol)
		seq0 := make([]string, 1600000)
		for i := range seq0 {
			seq0[i] = chunk0Keys[i%128]
		}
		att0, _, _, _, _ := runConcurrent(seq0, 8, 80,
			func(k string) bool { _, _, ok := c.Get("GET", k, nil); return ok },
			func(k string) { c.Add("GET", k, nil, nil) })
		_, _, drops0, ratio0 := c.Stats()
		c.Close()
		eff0 := att0 * float64(1600000-drops0) / 1600000.0
		dropPct0 := float64(drops0) / float64(1600000/5) * 100

		// Single-key upsert
		cs := liteLRU.NewLRUCacheWithPolicy(1024, 4, pol)
		seqS := make([]string, 1600000)
		for i := range seqS {
			seqS[i] = "/single-hot"
		}
		atts, _, _, _, _ := runConcurrent(seqS, 8, 0,
			func(k string) bool { _, _, ok := cs.Get("GET", k, nil); return ok },
			func(k string) { cs.Add("GET", k, nil, nil) })
		_, _, dropsS, _ := cs.Stats()
		cs.Close()
		effS := atts * float64(1600000-dropsS) / 1600000.0
		dropPctS := float64(dropsS) / 1600000.0 * 100

		fmt.Printf("%-8s | %5.2fM/%5.2fM/%5.2f%%/%5.2f%% | %5.2fM / %5.2fM / %6.2f%%\n",
			pol, att0/1e6, eff0/1e6, dropPct0, ratio0*100, atts/1e6, effS/1e6, dropPctS)
	}
}

func benchMemory(policies []liteLRU.EvictionPolicy) {
	fmt.Println("\n=== 4. MEMORY FOOTPRINT (cap=65,536 populated, post-GC) ===")
	fmt.Printf("%-8s | %-18s | %-14s | %-12s | %-12s\n",
		"Policy", "Logical Ctrl/Slot", "Allocated Slab", "HeapAlloc", "HeapInuse")
	keys := make([]string, 65536)
	for i := range keys {
		keys[i] = fmt.Sprintf("/mem/%d", i)
	}
	ctrlBytes := map[liteLRU.EvictionPolicy]int{
		liteLRU.PolicyCLOCK:     64 + 8 + 8 + 8 + 4,     // sigs + valid + f0(A) + writing + hand = 92 B/chunk
		liteLRU.PolicyCLOCKPlus: 64 + 8 + 8 + 8 + 8 + 4, // + f1(F) = 100 B/chunk
		liteLRU.PolicyS3FIFO:    128,                    // + prob, ghost0, ghost1, handM = 128 B/chunk
	}
	for _, pol := range policies {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		c := liteLRU.NewLRUCacheWithPolicy(65536, 4, pol)
		for _, k := range keys {
			c.Add("GET", k, nil, nil)
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(c)
		slabBytes := (65536 / 64) * 128
		fmt.Printf("%-8s | %.2f B (%.2f B tot) | %6d KiB     | %6.2f MiB   | %6.2f MiB\n",
			pol, float64(ctrlBytes[pol])/64.0, 8.0+float64(ctrlBytes[pol])/64.0,
			slabBytes/1024, float64(after.HeapAlloc)/(1<<20), float64(after.HeapInuse)/(1<<20))
		c.Close()
	}
	_ = unsafe.Sizeof(0)
}

func runCASOnly(policies []liteLRU.EvictionPolicy) {
	fmt.Println("=== 5. CAS CONTENTION PROFILE (-tags casmetrics, 8 workers, 1.6M ops) ===")
	const totalOps = 1600000
	zipf100k := genZipfOps(103, 1.001, 100000, totalOps)
	zipf1k := genZipfOps(101, 1.001, 1000, totalOps)
	chunk0Keys := make([]string, 0, 128)
	for i := 0; len(chunk0Keys) < 128; i++ {
		p := fmt.Sprintf("/chunk0/%d", i)
		if liteLRU.HashRoute("GET", p)%16 == 0 {
			chunk0Keys = append(chunk0Keys, p)
		}
	}
	seq0 := make([]string, totalOps)
	for i := range seq0 {
		seq0[i] = chunk0Keys[i%128]
	}

	type casCase struct {
		name   string
		getPct int
		seq    []string
	}
	cases := []casCase{
		{"Zipf 80/20 (cap=1k, N=100k)", 80, zipf100k},
		{"Direct Write 20/80 (cap=1k, N=1k)", 20, zipf1k},
		{"Chunk-0 128-key 80/20 (cap=1k)", 80, seq0},
	}

	for _, cc := range cases {
		fmt.Printf("\n[%s]\n", cc.name)
		fmt.Printf("%-8s | %-11s | %-10s | %-7s | %-9s | %-10s | %-9s | %-7s\n",
			"Policy", "ClaimAtt", "ClaimFail", "Fail%", "SigFail", "AgingSwp", "Exhaust", "Drops")
		for _, pol := range policies {
			liteLRU.ResetCASMetrics()
			c := liteLRU.NewLRUCacheWithPolicy(1024, 4, pol)
			runConcurrent(cc.seq, 8, cc.getPct,
				func(k string) bool { _, _, ok := c.Get("GET", k, nil); return ok },
				func(k string) { c.Add("GET", k, nil, nil) })
			_, _, drops, _ := c.Stats()
			c.Close()
			st := liteLRU.ReadCASMetrics()
			failPct := 0.0
			if st.ClaimAttempts > 0 {
				failPct = float64(st.ClaimFailures) / float64(st.ClaimAttempts) * 100
			}
			fmt.Printf("%-8s | %-11d | %-10d | %5.2f%% | %-9d | %-10d | %-9d | %-7d\n",
				pol, st.ClaimAttempts, st.ClaimFailures, failPct, st.SigCASFailures, st.AgingSweeps, st.VictimExhausted, drops)
		}
	}
}

func main() {
	casOnly := flag.Bool("cas-only", false, "run CAS contention instrumentation (requires -tags casmetrics)")
	skipZipf := flag.Bool("skip-zipf", false, "skip section 1 Zipf hit-rate sweep")
	flag.Parse()

	policies := []liteLRU.EvictionPolicy{
		liteLRU.PolicyCLOCK,
		liteLRU.PolicyCLOCKPlus,
		liteLRU.PolicyS3FIFO,
	}

	if *casOnly {
		runCASOnly(policies)
		return
	}

	if !*skipZipf {
		benchZipfHitRates(policies)
	}
	benchConcurrentMatrix(policies)
	benchAdversarial(policies)
	benchMemory(policies)

	// Run CAS metrics cleanly in an isolated -tags casmetrics subprocess so zero overhead affects throughput above.
	fmt.Println()
	cmd := exec.Command("go", "run", "-tags", "casmetrics", "./policy_compare", "-cas-only")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("CAS subprocess failed: %v\n", err)
	}
}
