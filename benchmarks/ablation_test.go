package main

import (
	"fmt"
	"math/bits"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
	"github.com/xDarkicex/liteLRU"
)

// NoPadCache: Removes the _ [60]byte padding from statStripe and Seqlock
type slotStateNoPad struct {
	seq atomic.Uint32
}
type statStripeNoPad struct {
	hits   atomic.Int64
	misses atomic.Int64
}

// NoMmapCache: Replaces memory.HashMap with map[uint64]uint32
type NoMmapCache struct {
	liteLRU.LRUCache // Embed to avoid rewriting everything, but we will redefine the methods for ablation
}

// NoBitmaskCache: Replaces CTZ with linear scan
type NoBitmaskCache struct {
	liteLRU.LRUCache
}

// Since we cannot easily monkey-patch Go structs, we will benchmark standard liteLRU, 
// and we will rely on a simplified ablation harness.
// Actually, it's faster to just copy the liteLRU code into one file and tweak it.

func main() {
	fmt.Println("Running Ablation Study...")

	const numKeys = 1000
	const numOps = 1600000
	const capacity = 1024
	const numWorkers = 8

	keys := make([]string, numKeys)
	for i := 0; i < numKeys; i++ {
		keys[i] = fmt.Sprintf("GET:/api/v1/resource/%d", i)
	}

	ops := make([]string, numOps)
	for i := 0; i < numOps; i++ {
		r := rand.Float64()
		if r < 0.7 {
			ops[i] = keys[i%200]
		} else {
			ops[i] = keys[i%numKeys]
		}
	}

	// Baseline liteLRU
	fmt.Println("\n--- liteLRU (Full Architecture) ---")
	fullCache := liteLRU.NewLRUCache(capacity, 10)
	runBench(ops, numWorkers, func(key string) {
		fullCache.Get("GET", key, nil)
	}, func(key string) {
		fullCache.Add("GET", key, nil, nil)
	})
	
	// We will run the degraded versions that I generated via bash
}

func runBench(ops []string, numWorkers int, getFunc, addFunc func(string)) {
	var wg sync.WaitGroup
	wg.Add(numWorkers)
	opsPerWorker := len(ops) / numWorkers
	start := time.Now()
	for w := 0; w < numWorkers; w++ {
		go func(w int) {
			defer wg.Done()
			startIdx := w * opsPerWorker
			endIdx := startIdx + opsPerWorker
			for i := startIdx; i < endIdx; i++ {
				if i%5 != 0 {
					getFunc(ops[i])
				} else {
					addFunc(ops[i])
				}
			}
		}(w)
	}
	wg.Wait()
	duration := time.Since(start)
	fmt.Printf("Total Time: %v\n", duration)
	fmt.Printf("Ops/sec   : %.2f ops/sec\n", float64(len(ops))/duration.Seconds())
}
