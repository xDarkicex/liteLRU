package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xDarkicex/liteLRU"
)

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

func main() {
	fmt.Println("Running Adversarial & Controlled Stress Benchmarks...")

	// 1. Single-set worst-case contention (all keys hash to chunk 0)
	{
		c := liteLRU.NewLRUCache(1024, 10)
		numGroups := uint64(1024 / 64)
		var singleSetKeys []string
		for i := 0; len(singleSetKeys) < 128; i++ {
			p := fmt.Sprintf("/hotset/%d", i)
			if hashRoute("GET", p)%numGroups == 0 {
				singleSetKeys = append(singleSetKeys, p)
			}
		}

		const workers = 8
		const opsPerWorker = 200000
		start := time.Now()
		var wg sync.WaitGroup
		wg.Add(workers)
		for w := 0; w < workers; w++ {
			go func(id int) {
				defer wg.Done()
				for i := 0; i < opsPerWorker; i++ {
					k := singleSetKeys[(id*37+i)%len(singleSetKeys)]
					if i%5 != 0 {
						c.Get("GET", k, nil)
					} else {
						c.Add("GET", k, nil, nil)
					}
				}
			}(w)
		}
		wg.Wait()
		dur := time.Since(start)
		_, _, drops, ratio := c.Stats()
		totalOps := workers * opsPerWorker
		addOps := totalOps / 5
		fmt.Printf("\n[1] Single-Set Contention (128 keys -> 1 chunk, 80/20 Get/Add):\n")
		fmt.Printf("    Attempted: %d ops/s | Effective: %d ops/s | Drops: %d (%.2f%% of Adds) | Hit Rate: %.2f%%\n",
			int(float64(totalOps)/dur.Seconds()),
			int(float64(totalOps-int(drops))/dur.Seconds()),
			drops, float64(drops)/float64(addOps)*100, ratio*100)
		c.Close()
	}

	// 2. 64 vs 65 keys in one set (associativity boundary)
	for _, nKeys := range []int{64, 65} {
		c := liteLRU.NewLRUCache(1024, 10)
		numGroups := uint64(1024 / 64)
		var keys []string
		for i := 0; len(keys) < nKeys; i++ {
			p := fmt.Sprintf("/bound/%d", i)
			if hashRoute("GET", p)%numGroups == 0 {
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
		fmt.Printf("\n[2] Associativity Boundary (%d keys in 1 set, sequential loop x1000): Hit Rate = %.2f%%\n",
			nKeys, float64(hits)/float64(1000*nKeys)*100)
		c.Close()
	}

	// 3. Controlled occupancy (1%, 50%, 100%) - separating 100% Get vs 80/20 Get/Add
	for _, occPct := range []int{1, 50, 100} {
		nFill := 1024 * occPct / 100
		if nFill < 1 {
			nFill = 1
		}
		const workers = 8
		const opsPerWorker = 200000
		totalOps := workers * opsPerWorker

		// 3a. Lookup-only (100% Get)
		cGet := liteLRU.NewLRUCache(1024, 10)
		keys := make([]string, nFill)
		for i := 0; i < nFill; i++ {
			keys[i] = fmt.Sprintf("/occ/%d", i)
			cGet.Add("GET", keys[i], nil, nil)
		}
		startGet := time.Now()
		var wgGet sync.WaitGroup
		wgGet.Add(workers)
		for w := 0; w < workers; w++ {
			go func(id int) {
				defer wgGet.Done()
				for i := 0; i < opsPerWorker; i++ {
					cGet.Get("GET", keys[(id*17+i)%len(keys)], nil)
				}
			}(w)
		}
		wgGet.Wait()
		durGet := time.Since(startGet)
		cGet.Close()

		// 3b. Mixed (80/20 Get/Add)
		cMix := liteLRU.NewLRUCache(1024, 10)
		for i := 0; i < nFill; i++ {
			cMix.Add("GET", keys[i], nil, nil)
		}
		startMix := time.Now()
		var wgMix sync.WaitGroup
		wgMix.Add(workers)
		for w := 0; w < workers; w++ {
			go func(id int) {
				defer wgMix.Done()
				for i := 0; i < opsPerWorker; i++ {
					k := keys[(id*17+i)%len(keys)]
					if i%5 != 0 {
						cMix.Get("GET", k, nil)
					} else {
						cMix.Add("GET", k, nil, nil)
					}
				}
			}(w)
		}
		wgMix.Wait()
		durMix := time.Since(startMix)
		_, _, drops, ratio := cMix.Stats()
		cMix.Close()

		fmt.Printf("\n[3] Occupancy %3d%% (%4d keys): 100%% Get = %d ops/s | 80/20 Get/Add = %d ops/s (Drops: %d, Hit: %.2f%%)\n",
			occPct, nFill,
			int(float64(totalOps)/durGet.Seconds()),
			int(float64(totalOps)/durMix.Seconds()),
			drops, ratio*100)
	}

	// 4. Sequential scan larger than capacity (scan resistance)
	{
		c := liteLRU.NewLRUCache(1024, 10)
		const hotCount = 256
		const scanCount = 2048
		for i := 0; i < hotCount; i++ {
			k := fmt.Sprintf("/hot/%d", i)
			c.Add("GET", k, nil, nil)
			c.Get("GET", k, nil)
		}
		for i := 0; i < scanCount; i++ {
			k := fmt.Sprintf("/scan/%d", i)
			if _, _, ok := c.Get("GET", k, nil); !ok {
				c.Add("GET", k, nil, nil)
			}
		}
		retained := 0
		for i := 0; i < hotCount; i++ {
			if _, _, ok := c.Get("GET", fmt.Sprintf("/hot/%d", i), nil); ok {
				retained++
			}
		}
		fmt.Printf("\n[4] Sequential Scan Resistance (256 hot keys followed by 2048 scan keys, cap=1024): Retained = %d/%d (%.2f%%)\n",
			retained, hotCount, float64(retained)/float64(hotCount)*100)
		c.Close()
	}

	// 5. Concurrent updates to one key
	{
		c := liteLRU.NewLRUCache(1024, 10)
		c.Add("GET", "/single-hot", nil, nil)
		const workers = 8
		const opsPerWorker = 200000
		start := time.Now()
		var wg sync.WaitGroup
		wg.Add(workers)
		for w := 0; w < workers; w++ {
			go func() {
				defer wg.Done()
				for i := 0; i < opsPerWorker; i++ {
					c.Add("GET", "/single-hot", nil, nil)
				}
			}()
		}
		wg.Wait()
		dur := time.Since(start)
		_, _, drops, _ := c.Stats()
		totalOps := workers * opsPerWorker
		fmt.Printf("\n[5] Concurrent Single-Key Upsert (8 workers, 1.6M Adds): Attempted: %d ops/s | Effective: %d ops/s | Drops: %d (%.2f%%)\n",
			int(float64(totalOps)/dur.Seconds()),
			int(float64(totalOps-int(drops))/dur.Seconds()),
			drops, float64(drops)/float64(totalOps)*100)
		c.Close()
	}

	// 6. Increasing key length (Get hit latency / throughput)
	for _, keyLen := range []int{16, 64, 256, 1024} {
		c := liteLRU.NewLRUCache(1024, 10)
		keys := make([]string, 512)
		pad := strings.Repeat("x", keyLen-12)
		for i := 0; i < len(keys); i++ {
			keys[i] = fmt.Sprintf("/k/%08d/%s", i, pad)
			c.Add("GET", keys[i], nil, nil)
		}
		const workers = 8
		const opsPerWorker = 200000
		start := time.Now()
		var wg sync.WaitGroup
		wg.Add(workers)
		for w := 0; w < workers; w++ {
			go func(id int) {
				defer wg.Done()
				for i := 0; i < opsPerWorker; i++ {
					c.Get("GET", keys[(id*13+i)%len(keys)], nil)
				}
			}(w)
		}
		wg.Wait()
		dur := time.Since(start)
		totalOps := workers * opsPerWorker
		fmt.Printf("\n[6] Key Length %4d bytes (100%% Get, 8 workers): %d ops/s\n",
			keyLen, int(float64(totalOps)/dur.Seconds()))
		c.Close()
	}
}
