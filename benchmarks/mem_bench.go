package main

import (
	"fmt"
	"runtime"

	"github.com/maypok86/otter"
	"github.com/xDarkicex/liteLRU"
)

func main() {
	capacity := 1_000_000
	fmt.Printf("Measuring memory overhead for %d entries...\n\n", capacity)

	var m1, m2 runtime.MemStats

	// 1. liteLRU
	runtime.GC()
	runtime.ReadMemStats(&m1)
	
	lite := liteLRU.NewLRUCache(capacity, 10)
	
	runtime.ReadMemStats(&m2)
	liteAlloc := m2.Alloc - m1.Alloc
	fmt.Printf("liteLRU (Heap Alloc):   %d bytes (%.2f MB)\n", liteAlloc, float64(liteAlloc)/1024/1024)
	fmt.Printf("liteLRU Sys (Includes Mmap): %.2f MB\n", float64(m2.Sys-m1.Sys)/1024/1024)

	// Keep alive
	fmt.Println("\nMeasuring Otter Memory...")
	runtime.GC()
	runtime.ReadMemStats(&m1)

	otterCache, _ := otter.MustBuilder[string, any](capacity).Build()

	for i := 0; i < capacity; i++ {
		key := fmt.Sprintf("route-destination-%d", i)
		otterCache.Set(key, nil)
	}

	runtime.GC()
	runtime.ReadMemStats(&m2)
	fmt.Printf("Otter     Allocated: %v MB\n", (m2.Alloc-m1.Alloc)/1024/1024)
	
	_ = lite
}
