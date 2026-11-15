package liteLRU

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestZeroAllocGet(t *testing.T) {
	c := NewLRUCache(128, 10)
	defer c.Close()

	params := []Param{{Key: "id", Value: "42"}, {Key: "role", Value: "admin"}}
	c.Add("GET", "/users/42", func() {}, params)

	var dst [4]Param
	allocsHit := testing.AllocsPerRun(1000, func() {
		_, p, ok := c.Get("GET", "/users/42", dst[:0])
		if !ok || len(p) != 2 {
			t.Fatalf("unexpected hit result: ok=%v len=%d", ok, len(p))
		}
	})
	if allocsHit != 0 {
		t.Fatalf("expected 0 allocs on Get hit, got %v", allocsHit)
	}

	allocsMiss := testing.AllocsPerRun(1000, func() {
		_, _, ok := c.Get("GET", "/users/missing", dst[:0])
		if ok {
			t.Fatal("unexpected hit on missing key")
		}
	})
	if allocsMiss != 0 {
		t.Fatalf("expected 0 allocs on Get miss, got %v", allocsMiss)
	}
}

func TestConcurrentParamsClosuresAndClear(t *testing.T) {
	// Single 64-slot chunk maximizes slot contention, evictions, and updates.
	c := NewLRUCache(64, 10)
	defer c.Close()

	const workers = 8
	const iters = 20000
	const keySpace = 128

	var tornCount atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers + 1)

	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			var dst [4]Param
			for i := 0; i < iters; i++ {
				k := (id*31 + i) % keySpace
				method := "GET"
				if k%2 == 1 {
					method = "POST"
				}
				path := fmt.Sprintf("/items/%d/subpath/%d", k, k*7)

				if i%3 == 0 {
					expected := method + ":" + path
					var marker atomic.Int64
					marker.Store(int64(len(expected)))
					handler := func() {
						if marker.Load() != int64(len(expected)) {
							tornCount.Add(1)
						}
					}
					params := []Param{
						{Key: "route", Value: expected},
						{Key: "worker", Value: fmt.Sprintf("w%d", id)},
					}
					// Vary param slice length to stress capacity transitions.
					if i%2 == 0 {
						params = params[:1]
					}
					c.Add(method, path, handler, params)
				} else {
					h, p, ok := c.Get(method, path, dst[:0])
					if ok {
						if len(p) == 0 || p[0].Value != method+":"+path {
							tornCount.Add(1)
						}
						if h != nil {
							h()
						}
					}
				}
			}
		}(w)
	}

	// Periodic concurrent Clear() calls.
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			c.Clear()
		}
	}()

	wg.Wait()
	if tornCount.Load() != 0 {
		t.Fatalf("observed %d torn key/param reads", tornCount.Load())
	}
}

func TestConcurrentDuplicateInsertAndUpdate(t *testing.T) {
	c := NewLRUCache(64, 10)
	defer c.Close()

	const workers = 16
	const iters = 5000
	var torn atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)

	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			var dst [2]Param
			for i := 0; i < iters; i++ {
				tag := fmt.Sprintf("w%d-i%d", id, i)
				c.Add("GET", "/hot/key", nil, []Param{
					{Key: "k1", Value: tag},
					{Key: "k2", Value: tag},
				})
				_, p, ok := c.Get("GET", "/hot/key", dst[:0])
				if ok && (len(p) != 2 || p[0].Value != p[1].Value) {
					torn.Add(1)
				}
			}
		}(w)
	}

	wg.Wait()
	if torn.Load() != 0 {
		t.Fatalf("observed %d torn param pairs under concurrent duplicate upsert", torn.Load())
	}
}

func TestSingleSetAssociativityBoundary(t *testing.T) {
	// 128 capacity => 2 sets (groups 0 and 1), 64 slots per set.
	c := NewLRUCache(128, 4)
	defer c.Close()

	// Find 65 distinct paths that all hash to group 0.
	var group0Paths []string
	for i := 0; len(group0Paths) < 65; i++ {
		p := fmt.Sprintf("/collision/%d", i)
		if hashRoute("GET", p)%uint64(c.numGroups) == 0 {
			group0Paths = append(group0Paths, p)
		}
	}

	// Fill all 64 ways of group 0.
	for i := 0; i < 64; i++ {
		c.Add("GET", group0Paths[i], nil, []Param{{Key: "idx", Value: fmt.Sprintf("%d", i)}})
	}

	var dst [2]Param
	for i := 0; i < 64; i++ {
		if _, _, ok := c.Get("GET", group0Paths[i], dst[:0]); !ok {
			t.Fatalf("expected all 64 ways in set 0 to fit before 65th insert; missed key %d", i)
		}
	}

	// Adding a 65th key to group 0 must evict exactly one of the 64 keys.
	c.Add("GET", group0Paths[64], nil, []Param{{Key: "idx", Value: "64"}})

	present := 0
	for i := 0; i < 65; i++ {
		if _, _, ok := c.Get("GET", group0Paths[i], dst[:0]); ok {
			present++
		}
	}
	if present != 64 {
		t.Fatalf("expected 64 entries in set 0 after 65th insert, got %d", present)
	}
	if _, _, ok := c.Get("GET", group0Paths[64], dst[:0]); !ok {
		t.Fatal("expected 65th key to be present after eviction")
	}
}
