// Package liteLRU_test provides benchmark tests for the liteLRU cache implementation.
// It contains comprehensive benchmarks to measure performance characteristics
// under various workloads, cache sizes, and hit ratios.
package liteLRU

import (
	"fmt"
	"math/rand"
	"strconv"
	"testing"
	"time"
)

// BenchmarkLRUCache runs a comprehensive suite of benchmarks for the LRUCache implementation.
// It tests various cache sizes, hit ratios, parameter counts, and operation types (Add, Get, Mixed)
// to provide a complete performance profile of the cache under different conditions.
func BenchmarkLRUCache(b *testing.B) {
	// Create caches with different capacities to test
	// These sizes represent typical use cases from small to large caches
	cacheSizes := []int{128, 512, 1024, 4096}

	// Different request patterns with varying hit ratios and parameter counts
	// to simulate different real-world usage scenarios
	benchmarks := []struct {
		name      string  // Descriptive name for the benchmark
		hitRatio  float64 // Theoretical hit ratio based on how we generate keys
		paramSize int     // Average number of parameters per request
	}{
		{"HighHitRatio_FewParams", 0.9, 3},   // High cache hit rate with few parameters
		{"HighHitRatio_ManyParams", 0.9, 12}, // High cache hit rate with many parameters
		{"LowHitRatio_FewParams", 0.2, 3},    // Low cache hit rate with few parameters
		{"LowHitRatio_ManyParams", 0.2, 12},  // Low cache hit rate with many parameters
		{"MixedParams", 0.6, 6},              // Medium hit rate with medium parameter count
	}

	// HTTP methods to use for requests
	// These represent the standard HTTP methods used in RESTful APIs
	methods := []string{"GET", "POST", "PUT", "DELETE", "PATCH"}

	// Generate a range of static paths for testing
	// These paths simulate API endpoints with unique identifiers
	var paths []string
	for i := 0; i < 10000; i++ {
		paths = append(paths, fmt.Sprintf("/api/resource/%d", i))
	}

	// Seed random number generator for reproducible tests
	rand.Seed(time.Now().UnixNano())

	// Create dummy handler for cache entries
	// This is a no-op function that simulates a route handler
	dummyHandler := func() {}

	// Run benchmarks for each cache size and benchmark pattern
	for _, size := range cacheSizes {
		for _, bm := range benchmarks {
			// Benchmark Add operations
			// This measures the performance of adding or updating entries in the cache
			b.Run(fmt.Sprintf("Add_Size%d_%s", size, bm.name), func(b *testing.B) {
				cache := NewLRUCache(size, 20)
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					// Select random HTTP method
					method := methods[rand.Intn(len(methods))]

					// Select path based on desired hit ratio
					pathIdx := 0
					if rand.Float64() > bm.hitRatio {
						// For misses, use a wider range of paths than the cache size
						pathIdx = rand.Intn(size * 5)
						// Add bounds checking to prevent index out of range
						if pathIdx >= len(paths) {
							pathIdx = pathIdx % len(paths)
						}
					} else {
						// For hits, stick to a smaller range that's likely in cache
						pathIdx = rand.Intn(size / 2)
						if pathIdx >= len(paths) {
							pathIdx = pathIdx % len(paths)
						}
					}

					path := paths[pathIdx]

					// Generate random parameters with slight variability around the target size
					paramCount := bm.paramSize + rand.Intn(5) - 2 // +/- 2 from average
					if paramCount < 1 {
						paramCount = 1 // Ensure at least one parameter
					}

					params := make([]Param, paramCount)
					for j := 0; j < paramCount; j++ {
						params[j] = Param{
							Key:   "param" + strconv.Itoa(j),
							Value: "value" + strconv.Itoa(j),
						}
					}

					cache.Add(method, path, dummyHandler, params)
				}
			})

			// Benchmark Get operations
			// This measures the performance of retrieving entries from the cache
			b.Run(fmt.Sprintf("Get_Size%d_%s", size, bm.name), func(b *testing.B) {
				// First populate the cache with entries to simulate a warmed-up cache
				cache := NewLRUCache(size, 20)
				for i := 0; i < size; i++ {
					method := methods[i%len(methods)]

					// Add bounds checking to prevent index out of range
					pathIdx := i
					if pathIdx >= len(paths) {
						pathIdx = pathIdx % len(paths)
					}
					path := paths[pathIdx]

					params := make([]Param, bm.paramSize)
					for j := 0; j < bm.paramSize; j++ {
						params[j] = Param{
							Key:   "param" + strconv.Itoa(j),
							Value: "value" + strconv.Itoa(j),
						}
					}
					cache.Add(method, path, dummyHandler, params)
				}

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					method := methods[rand.Intn(len(methods))]

					// Select path based on desired hit ratio
					pathIdx := 0
					if rand.Float64() > bm.hitRatio {
						// For misses, use a path outside of what we've added
						pathIdx = size + rand.Intn(size*4)
						if pathIdx >= len(paths) {
							pathIdx = pathIdx % len(paths)
						}
					} else {
						// For hits, use a path we added earlier
						pathIdx = rand.Intn(size)
						if pathIdx >= len(paths) {
							pathIdx = pathIdx % len(paths)
						}
					}

					path := paths[pathIdx]
					_, _, _ = cache.Get(method, path)
				}

				// Report actual hit/miss ratio for validation
				// This helps verify that our test is achieving the target hit ratio
				h, m, ratio := cache.Stats()
				b.ReportMetric(ratio*100, "hit%")
				b.ReportMetric(float64(h+m)/float64(b.N)*100, "coverage%")
			})

			// Benchmark mixed operations (75% gets, 25% adds)
			// This simulates a more realistic workload with a mix of reads and writes
			b.Run(fmt.Sprintf("Mixed_Size%d_%s", size, bm.name), func(b *testing.B) {
				cache := NewLRUCache(size, 20)

				// Prepopulate with some entries (half the capacity)
				// This simulates a partially warmed cache
				for i := 0; i < size/2; i++ {
					method := methods[i%len(methods)]

					// Add bounds checking to prevent index out of range
					pathIdx := i
					if pathIdx >= len(paths) {
						pathIdx = pathIdx % len(paths)
					}
					path := paths[pathIdx]

					params := make([]Param, bm.paramSize)
					for j := 0; j < bm.paramSize; j++ {
						params[j] = Param{
							Key:   "param" + strconv.Itoa(j),
							Value: "value" + strconv.Itoa(j),
						}
					}
					cache.Add(method, path, dummyHandler, params)
				}

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					method := methods[rand.Intn(len(methods))]

					// 75% gets, 25% adds - typical read-heavy workload
					if rand.Float64() < 0.75 {
						// Get operation
						pathIdx := 0
						if rand.Float64() > bm.hitRatio {
							pathIdx = size + rand.Intn(size)
							if pathIdx >= len(paths) {
								pathIdx = pathIdx % len(paths)
							}
						} else {
							pathIdx = rand.Intn(size / 2)
							if pathIdx >= len(paths) {
								pathIdx = pathIdx % len(paths)
							}
						}

						path := paths[pathIdx]
						_, _, _ = cache.Get(method, path)
					} else {
						// Add operation
						pathIdx := rand.Intn(len(paths))
						path := paths[pathIdx]

						paramCount := bm.paramSize + rand.Intn(5) - 2
						if paramCount < 1 {
							paramCount = 1
						}

						params := make([]Param, paramCount)
						for j := 0; j < paramCount; j++ {
							params[j] = Param{
								Key:   "param" + strconv.Itoa(j),
								Value: "value" + strconv.Itoa(j),
							}
						}

						cache.Add(method, path, dummyHandler, params)
					}
				}

				// Report the actual hit ratio achieved during the test
				_, _, ratio := cache.Stats()
				b.ReportMetric(ratio*100, "hit%")
			})
		}
	}
}

// BenchmarkParamPooling tests the performance of parameter pooling with a realistic
// workload distribution. This benchmark simulates real-world API usage patterns with
// varying parameter counts based on observed frequencies.
func BenchmarkParamPooling(b *testing.B) {
	// HTTP methods to use for requests
	methods := []string{"GET", "POST", "PUT", "DELETE", "PATCH"}

	// Generate a range of static paths for testing
	var paths []string
	for i := 0; i < 10000; i++ {
		paths = append(paths, fmt.Sprintf("/api/resource/%d", i))
	}

	// Create dummy handler for cache entries
	dummyHandler := func() {}

	// Create a fixed workload based on real-world parameter distributions
	type paramDistribution struct {
		count     int     // Number of parameters
		frequency float64 // Percentage of requests with this many params
	}

	// This simulates a realistic API with different parameter counts
	// based on observed distribution patterns in production environments
	paramDistributions := []paramDistribution{
		{2, 0.3},  // 30% of requests have 2 params
		{4, 0.25}, // 25% of requests have 4 params
		{6, 0.2},  // 20% of requests have 6 params
		{8, 0.15}, // 15% of requests have 8 params
		{12, 0.1}, // 10% of requests have 12 params
	}

	// Generate workload with realistic parameter distribution
	const workloadSize = 10000
	workload := make([]struct {
		method string
		path   string
		params []Param
	}, workloadSize)

	for i := 0; i < workloadSize; i++ {
		// Pick random distribution based on frequency
		roll := rand.Float64()
		cumulative := 0.0
		paramCount := 4 // default

		for _, dist := range paramDistributions {
			cumulative += dist.frequency
			if roll <= cumulative {
				paramCount = dist.count
				break
			}
		}

		workload[i].method = methods[rand.Intn(len(methods))]
		workload[i].path = paths[rand.Intn(len(paths))]
		workload[i].params = make([]Param, paramCount)

		for j := 0; j < paramCount; j++ {
			workload[i].params[j] = Param{
				Key:   "param" + strconv.Itoa(j),
				Value: "value" + strconv.Itoa(j),
			}
		}
	}

	cache := NewLRUCache(1024, 20)
	b.ResetTimer()

	// Run the real-world workload benchmark
	// This tests the cache with a distribution of operations that matches
	// observed patterns in production API servers
	b.Run("RealWorldWorkload", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			idx := i % workloadSize
			entry := workload[idx]

			if rand.Float64() < 0.75 {
				// 75% gets - typical read-heavy API workload
				_, _, _ = cache.Get(entry.method, entry.path)
			} else {
				// 25% adds - write operations
				cache.Add(entry.method, entry.path, dummyHandler, entry.params)
			}
		}
	})
}
