package liteLRU

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestConcurrentLatency(t *testing.T) {
	methods := []string{"GET", "POST", "PUT", "DELETE", "PATCH"}
	var paths []string
	for i := 0; i < 1000; i++ {
		paths = append(paths, fmt.Sprintf("/api/resource/%d", i))
	}
	dummyHandler := func() {}

	cache := NewLRUCache(1024, 20)

	numWorkers := 8
	opsPerWorker := 200000
	totalOps := numWorkers * opsPerWorker

	// Pre-allocate to prevent allocation overhead during measurement
	durations := make([][]time.Duration, numWorkers)
	for i := range durations {
		durations[i] = make([]time.Duration, opsPerWorker)
	}

	var startWG sync.WaitGroup
	var doneWG sync.WaitGroup

	startWG.Add(1)
	doneWG.Add(numWorkers)

	for w := 0; w < numWorkers; w++ {
		go func(workerID int) {
			defer doneWG.Done()
			
			// Local random to avoid lock contention on global rand
			localSeed := uint32(workerID + 1)
			fastRand := func() uint32 {
				localSeed ^= localSeed << 13
				localSeed ^= localSeed >> 17
				localSeed ^= localSeed << 5
				return localSeed
			}

			startWG.Wait() // Wait for all workers to be ready

			for i := 0; i < opsPerWorker; i++ {
				r1 := fastRand()
				r2 := fastRand()
				method := methods[r1%uint32(len(methods))]
				path := paths[r1%uint32(len(paths))]
				isGet := (r2 % 100) < 80 // 80% Get

				start := time.Now()
				
				if isGet {
					cache.Get(method, path, nil)
				} else {
					cache.Add(method, path, dummyHandler, nil)
				}
				
				durations[workerID][i] = time.Since(start)
			}
		}(w)
	}

	fmt.Printf("Starting latency test with %d workers, %d total ops (80%% GET / 20%% ADD)...\n", numWorkers, totalOps)
	
	startTest := time.Now()
	startWG.Done() // release workers
	doneWG.Wait()
	totalDuration := time.Since(startTest)

	opsPerSec := float64(totalOps) / totalDuration.Seconds()

	// Aggregate and sort durations
	allDurations := make([]time.Duration, 0, totalOps)
	for _, d := range durations {
		allDurations = append(allDurations, d...)
	}
	sort.Slice(allDurations, func(i, j int) bool { return allDurations[i] < allDurations[j] })

	p50 := allDurations[int(float64(totalOps)*0.50)]
	p99 := allDurations[int(float64(totalOps)*0.99)]
	p999 := allDurations[int(float64(totalOps)*0.999)]
	p100 := allDurations[totalOps-1]
	
	// Atomic hits/misses check
	hits, misses, _, ratio := cache.Stats()

	fmt.Printf("\n--- LATENCY RESULTS ---\n")
	fmt.Printf("Total Time     : %v\n", totalDuration)
	fmt.Printf("Ops/sec        : %.2f ops/sec\n", opsPerSec)
	fmt.Printf("Cache Hits     : %d\n", hits)
	fmt.Printf("Cache Misses   : %d\n", misses)
	fmt.Printf("Hit Ratio      : %.2f%%\n", ratio*100)
	fmt.Printf("\n--- PERCENTILES ---\n")
	fmt.Printf("p50 (Median)   : %v\n", p50)
	fmt.Printf("p99            : %v\n", p99)
	fmt.Printf("p99.9          : %v\n", p999)
	fmt.Printf("Max (p100)     : %v\n", p100)
	fmt.Printf("-----------------------\n")
}
