package main

import (
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/maypok86/otter"
	"github.com/xDarkicex/liteLRU"
)

const workingSetSize = 100000
const numOps = 2000000
const warmupOps = 400000
const s = 0.8

func main() {
	fmt.Printf("Generating Zipfian distribution (s=%.2f) data...\n", s)
	
	pmf := make([]float64, workingSetSize)
	var sum float64
	for i := 1; i <= workingSetSize; i++ {
		p := 1.0 / math.Pow(float64(i), s)
		pmf[i-1] = p
		sum += p
	}
	
	cdf := make([]float64, workingSetSize)
	var csum float64
	for i := 0; i < workingSetSize; i++ {
		csum += pmf[i] / sum
		cdf[i] = csum
	}

	ops := make([]string, numOps)
	for i := 0; i < numOps; i++ {
		r := rand.Float64()
		
		// Binary search
		low, high := 0, workingSetSize-1
		var rank int
		for low <= high {
			mid := (low + high) / 2
			if cdf[mid] >= r {
				rank = mid
				high = mid - 1
			} else {
				low = mid + 1
			}
		}
		
		ops[i] = strconv.Itoa(rank)
	}

	capacities := []int{25000, 50000, 75000, 95000}

	for _, cap := range capacities {
		fmt.Printf("\n=== Cache Capacity: %d (%.0f%% of working set) ===\n", cap, float64(cap)/float64(workingSetSize)*100)

		// 1. liteLRU
		lite := liteLRU.NewLRUCache(cap, 5)
		var liteHits, liteMisses atomic.Uint64

		for i := 0; i < warmupOps; i++ {
			key := ops[i]
			if _, _, ok := lite.Get("GET", key); !ok {
				lite.Add("GET", key, nil, nil)
			}
		}

		var wg sync.WaitGroup
		wg.Add(8)
		chunkSize := (numOps - warmupOps) / 8
		for i := 0; i < 8; i++ {
			go func(start, end int) {
				for j := start; j < end; j++ {
					key := ops[j]
					if _, _, ok := lite.Get("GET", key); ok {
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
		otterCache, err := otter.MustBuilder[string, any](cap).Build()
		if err != nil {
			panic(err)
		}
		var otterHits, otterMisses atomic.Uint64

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
						otterCache.Set(key, nil)
					}
				}
				wg.Done()
			}(warmupOps+(i*chunkSize), warmupOps+((i+1)*chunkSize))
		}
		wg.Wait()
		fmt.Printf("Otter Hit Rate: %.2f%%\n", float64(otterHits.Load())/float64(measuredOps)*100)
	}
}
