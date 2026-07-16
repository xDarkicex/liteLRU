package main

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/dgraph-io/ristretto"
	"github.com/xDarkicex/liteLRU"
)

const workingSetSize = 100000
const numOps = 2000000

func main() {
	fmt.Println("Running Zipfian Hit-Rate Benchmarks...")

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

		var wg sync.WaitGroup
		wg.Add(8)
		chunkSize := numOps / 8
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
			}(i*chunkSize, (i+1)*chunkSize)
		}
		wg.Wait()
		fmt.Printf("liteLRU   Hit Rate: %.2f%%\n", float64(liteHits.Load())/float64(numOps)*100)

		// 2. Ristretto
		ristrettoCache, _ := ristretto.NewCache(&ristretto.Config{
			NumCounters: int64(cap * 10),
			MaxCost:     int64(cap),
			BufferItems: 64,
		})
		var ristHits, ristMisses atomic.Uint64

		wg.Add(8)
		for i := 0; i < 8; i++ {
			go func(start, end int) {
				for j := start; j < end; j++ {
					key := ops[j]
					if _, ok := ristrettoCache.Get(key); ok {
						ristHits.Add(1)
					} else {
						ristMisses.Add(1)
						ristrettoCache.Set(key, nil, 1)
					}
				}
				wg.Done()
			}(i*chunkSize, (i+1)*chunkSize)
		}
		wg.Wait()
		// Wait for ristretto background processing
		ristrettoCache.Wait()
		fmt.Printf("Ristretto Hit Rate: %.2f%%\n", float64(ristHits.Load())/float64(numOps)*100)
	}
}
