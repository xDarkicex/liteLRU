package main

import (
	"fmt"
	"runtime"

	"github.com/dgraph-io/ristretto"
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
	_ = lite

	// 2. Ristretto
	runtime.GC()
	runtime.ReadMemStats(&m1)
	
	rist, _ := ristretto.NewCache(&ristretto.Config{
		NumCounters: int64(capacity * 10),
		MaxCost:     int64(capacity),
		BufferItems: 64,
	})

	runtime.ReadMemStats(&m2)
	ristAlloc := m2.Alloc - m1.Alloc
	fmt.Printf("Ristretto (Heap Alloc): %d bytes (%.2f MB)\n", ristAlloc, float64(ristAlloc)/1024/1024)
	fmt.Printf("Ristretto Sys: %.2f MB\n", float64(m2.Sys-m1.Sys)/1024/1024)
	
	_ = rist
}
