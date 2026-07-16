package main

import (
	"container/list"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/xDarkicex/liteLRU"
)

func main() {
	fmt.Println("Running Baseline Benchmarks...")
	
	// Create keys for 70% hit rate setup (same as liteLRU test)
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
	fmt.Println("\n--- liteLRU ---")
	lCache := liteLRU.NewLRUCache(capacity, 10)
	runBench(ops, numWorkers, func(key string) {
		lCache.Get("GET", key)
	}, func(key string) {
		lCache.Add("GET", key, nil, nil)
	})

	// 2. Benchmark Ristretto
	fmt.Println("\n--- Ristretto ---")
	rCache, _ := ristretto.NewCache(&ristretto.Config{
		NumCounters: 1e7,     // number of keys to track frequency of (10M).
		MaxCost:     1024,    // maximum cost of cache (1KB).
		BufferItems: 64,      // number of keys per Get buffer.
	})
	runBench(ops, numWorkers, func(key string) {
		rCache.Get(key)
	}, func(key string) {
		rCache.Set(key, 1, 1)
	})

	// 3. Benchmark Mutex LRU
	fmt.Println("\n--- Mutex LRU ---")
	mCache := newMutexLRU(capacity)
	runBench(ops, numWorkers, func(key string) {
		mCache.Get(key)
	}, func(key string) {
		mCache.Add(key, 1)
	})
}

func runBench(ops []string, numWorkers int, getFunc, addFunc func(string)) {
	var wg sync.WaitGroup
	wg.Add(numWorkers)
	opsPerWorker := len(ops) / numWorkers
	
	latencies := make([][]time.Duration, numWorkers)
	
	start := time.Now()
	for w := 0; w < numWorkers; w++ {
		go func(w int) {
			defer wg.Done()
			startIdx := w * opsPerWorker
			endIdx := startIdx + opsPerWorker
			
			// Pre-allocate to avoid GC noise during benchmark
			localLatencies := make([]time.Duration, 0, opsPerWorker/100)
			
			for i := startIdx; i < endIdx; i++ {
				op := ops[i]
				
				var opStart time.Time
				sample := (i % 100 == 0) // sample 1% of operations
				if sample {
					opStart = time.Now()
				}
				
				// 80% GET, 20% ADD
				if i%5 != 0 {
					getFunc(op)
				} else {
					addFunc(op)
				}
				
				if sample {
					localLatencies = append(localLatencies, time.Since(opStart))
				}
			}
			latencies[w] = localLatencies
		}(w)
	}
	wg.Wait()
	duration := time.Since(start)
	
	// Aggregate and sort latencies
	var allLatencies []time.Duration
	for _, l := range latencies {
		allLatencies = append(allLatencies, l...)
	}
	sort.Slice(allLatencies, func(i, j int) bool {
		return allLatencies[i] < allLatencies[j]
	})
	
	p50 := allLatencies[len(allLatencies)*50/100]
	p99 := allLatencies[len(allLatencies)*99/100]
	
	fmt.Printf("Total Time: %v\n", duration)
	fmt.Printf("Ops/sec   : %.2f ops/sec\n", float64(len(ops))/duration.Seconds())
	fmt.Printf("p50 Latency: %v\n", p50)
	fmt.Printf("p99 Latency: %v\n", p99)
}

// MutexLRU implementation
type MutexLRU struct {
	sync.Mutex
	cap   int
	ll    *list.List
	cache map[string]*list.Element
}
type entry struct {
	key string
	val interface{}
}
func newMutexLRU(capacity int) *MutexLRU {
	return &MutexLRU{
		cap:   capacity,
		ll:    list.New(),
		cache: make(map[string]*list.Element),
	}
}
func (m *MutexLRU) Get(key string) interface{} {
	m.Lock()
	defer m.Unlock()
	if ele, hit := m.cache[key]; hit {
		m.ll.MoveToFront(ele)
		return ele.Value.(*entry).val
	}
	return nil
}
func (m *MutexLRU) Add(key string, value interface{}) {
	m.Lock()
	defer m.Unlock()
	if ele, hit := m.cache[key]; hit {
		m.ll.MoveToFront(ele)
		ele.Value.(*entry).val = value
		return
	}
	ele := m.ll.PushFront(&entry{key, value})
	m.cache[key] = ele
	if m.cap != 0 && m.ll.Len() > m.cap {
		if ent := m.ll.Back(); ent != nil {
			m.ll.Remove(ent)
			delete(m.cache, ent.Value.(*entry).key)
		}
	}
}
