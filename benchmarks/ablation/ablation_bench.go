package main

import (
	"ablation/nobitmask"
	"ablation/nommap"
	"ablation/nopad"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/xDarkicex/liteLRU"
)

func main() {
	fmt.Println("Running Ablation Benchmarks...")

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

	// 1. Benchmark liteLRU
	fmt.Println("\n--- liteLRU (Full Architecture) ---")
	lCache := liteLRU.NewLRUCache(capacity, 10)
	runBench(ops, numWorkers, func(key string) {
		lCache.Get("GET", key, nil)
	}, func(key string) {
		lCache.Add("GET", key, nil, nil)
	})

	// 2. Benchmark NoPad
	fmt.Println("\n--- liteLRU (No Padding) ---")
	npCache := nopad.NewLRUCache(capacity, 10)
	runBench(ops, numWorkers, func(key string) {
		npCache.Get("GET", key, nil)
	}, func(key string) {
		npCache.Add("GET", key, nil, nil)
	})

	// 3. Benchmark NoMmap
	fmt.Println("\n--- liteLRU (No Mmap / Go Map) ---")
	nmCache := nommap.NewLRUCache(capacity, 10)
	runBench(ops, numWorkers, func(key string) {
		nmCache.Get("GET", key, nil)
	}, func(key string) {
		nmCache.Add("GET", key, nil, nil)
	})

	// 4. Benchmark NoBitmask
	fmt.Println("\n--- liteLRU (No Bitmask / Linear Scan) ---")
	nbCache := nobitmask.NewLRUCache(capacity, 10)
	runBench(ops, numWorkers, func(key string) {
		nbCache.Get("GET", key, nil)
	}, func(key string) {
		nbCache.Add("GET", key, nil, nil)
	})
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
				op := ops[i]
				if i%5 != 0 {
					getFunc(op)
				} else {
					addFunc(op)
				}
			}
		}(w)
	}
	wg.Wait()
	duration := time.Since(start)

	fmt.Printf("Total Time: %v\n", duration)
	fmt.Printf("Ops/sec   : %.2f ops/sec\n", float64(len(ops))/duration.Seconds())
}
