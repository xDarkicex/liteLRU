package main

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/maypok86/otter"
	"github.com/xDarkicex/liteLRU"
)

const workingSetSize = 100000
const numOps = 2000000
const warmupOps = 400000 // 20% of operations for warmup

func main() {
	fmt.Println("Running Zipfian Hit-Rate Benchmarks (with warmup)...")

	// Generate Zipfian access sequence
	r := rand.New(rand.NewSource(42))
	zipf := rand.NewZipf(r, 1.001, 1, uint64(workingSetSize-1))

	ops := make([]string, numOps)
	for i := 0; i < numOps; i++ {
		ops[i] = strconv.FormatUint(zipf.Uint64(), 10)
	}

	capacities := []int{25000, 50000, 75000, 95000}

	for _, cap := range capacities {
		fmt.Printf("\n=== Cache Capacity: %d (%.0f%% of working set) ===\n", cap, float64(cap)/float64(workingSetSize)*100)

		// 1. liteLRU
		lite := liteLRU.NewLRUCache(cap, 5)
		var liteHits, liteMisses atomic.Uint64

		// Warmup liteLRU
		for i := 0; i < warmupOps; i++ {
			key := ops[i]
			if _, _, ok := lite.Get("GET", key, nil); !ok {
				lite.Add("GET", key, nil, nil)
			}
		}

		// Measured Phase
		var wg sync.WaitGroup
		wg.Add(8)
		chunkSize := (numOps - warmupOps) / 8
		for i := 0; i < 8; i++ {
			go func(start, end int) {
				for j := start; j < end; j++ {
					key := ops[j]
					if _, _, ok := lite.Get("GET", key, nil); ok {
						liteHits.Add(1)
					} else {
						liteMisses.Add(1)
						lite.Add("GET", key, nil, nil)
					}
				}
				wg.Done()
			}(warmupOps+(i*chunkSize), warmupOps+((i+1)*chunkSize))
		}
		wg.Wait()
		measuredOps := numOps - warmupOps
		fmt.Printf("liteLRU   Hit Rate: %.2f%%\n", float64(liteHits.Load())/float64(measuredOps)*100)

		// 2. Otter
		otterCache, err := otter.MustBuilder[string, any](cap).
			CollectStats().
			Build()
		if err != nil {
			panic(err)
		}
		var otterHits, otterMisses atomic.Uint64

		// Warmup Otter
		for i := 0; i < warmupOps; i++ {
			key := ops[i]
			if !otterCache.Has(key) {
				otterCache.Set(key, nil)
			}
		}

		wg.Add(8)
		for i := 0; i < 8; i++ {
			go func(start, end int) {
				for j := start; j < end; j++ {
					key := ops[j]
					if _, ok := otterCache.Get(key); ok {
						otterHits.Add(1)
					} else {
						otterMisses.Add(1)
						otterCache.Set(key, nil, nil)
					}
				}
				wg.Done()
			}(warmupOps+(i*chunkSize), warmupOps+((i+1)*chunkSize))
		}
		wg.Wait()
		fmt.Printf("Otter Hit Rate: %.2f%%\n", float64(otterHits.Load())/float64(measuredOps)*100)
	}
}
