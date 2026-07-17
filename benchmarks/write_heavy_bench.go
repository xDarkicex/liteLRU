package main

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/maypok86/otter"
	"github.com/xDarkicex/liteLRU"
)

const numKeys = 1000
const numOps = 1600000

func main() {
	fmt.Println("Running Write-Heavy Benchmarks...")

	// Create a Zipfian distribution of keys for the benchmark
	r := rand.New(rand.NewSource(42))
	zipf := rand.NewZipf(r, 1.001, 1, numKeys-1)

	keys := make([]string, numKeys)
	for i := 0; i < numKeys; i++ {
		keys[i] = strconv.Itoa(i)
	}

	workloads := []struct {
		name     string
		getRatio int // percentage
	}{
		{"50/50 Get/Add", 50},
		{"20/80 Get/Add", 20},
	}

	for _, wl := range workloads {
		fmt.Printf("\n=== %s ===\n", wl.name)

		// Generate ops based on getRatio
		type Op struct {
			isGet bool
			key   string
		}
		ops := make([]Op, numOps)
		for i := 0; i < numOps; i++ {
			ops[i] = Op{
				isGet: rand.Intn(100) < wl.getRatio,
				key:   keys[zipf.Uint64()],
			}
		}

		// liteLRU
		lite := liteLRU.NewLRUCache(1024, 10)
		start := time.Now()
		var wg sync.WaitGroup
		wg.Add(8)
		chunkSize := numOps / 8
		for i := 0; i < 8; i++ {
			go func(s, e int) {
				for j := s; j < e; j++ {
					if ops[j].isGet {
						lite.Get("GET", ops[j].key, nil)
					} else {
						lite.Add("GET", ops[j].key, nil, nil)
					}
				}
				wg.Done()
			}(i*chunkSize, (i+1)*chunkSize)
		}
		wg.Wait()
		duration := time.Since(start)
		fmt.Printf("liteLRU   Ops/sec: %d\n", int(float64(numOps)/duration.Seconds()))

		// Otter
		otterCache, _ := otter.MustBuilder[string, any](1024).Build()
		start = time.Now()
		wg.Add(8)
		for i := 0; i < 8; i++ {
			go func(s, e int) {
				for j := s; j < e; j++ {
					if ops[j].isGet {
						otterCache.Get(ops[j].key)
					} else {
						otterCache.Set(ops[j].key, nil, nil)
					}
				}
				wg.Done()
			}(i*chunkSize, (i+1)*chunkSize)
		}
		wg.Wait()
		duration = time.Since(start)
		fmt.Printf("Otter     Ops/sec: %d\n", int(float64(numOps)/duration.Seconds()))
	}
}
