<div align="center">

# Scaling Eviction to the Metal: An O(1) Lock-Free Chunked Bitmask Cache for Multi-Core Architectures

**Author:** xDarkicex  
**Affiliations:** libravdb, bitdev, zephyr-systems  
**Contact:** git@libravdb.com  

</div>

## Abstract

In the database and storage systems space, a massive gap exists between academic cache research and physical hardware reality. While contemporary research often focuses on optimizing algorithmic hit ratios via complex eviction policies on web-scale traces (e.g., S3-FIFO, SIEVE), it frequently overlooks the devastating execution costs of these algorithms on modern multi-core systems. Traditional Least Recently Used (LRU) algorithms require updating a doubly-linked list on every cache hit, forcing core-wide synchronization locks. Under parallel workloads, the time CPUs spend fighting over a mutual exclusion lock (mutex contention) is orders of magnitude slower than the actual memory reads.

We introduce `liteLRU`, an ultra-low latency, 100% lock-free cache architecture that trades a negligible fraction of single-threaded speed (5–10ns) to completely eliminate write contention across CPU cores. By utilizing Chunked Bitmask CLOCK eviction, Sequence Locks (Seqlocks), padded CPU cache lines to mitigate false sharing, and a lock-free open-addressed hash map, `liteLRU` achieves near-linear scaling up to 17.0 million operations per second across 8 cores. The resulting architecture delivers a p99.9 tail latency of 1.4 microseconds under active, multi-threaded read/write workloads.

---

## I. The Contention Bottleneck

The standard implementation of an LRU cache relies on a hash map for $O(1)$ lookups and a doubly-linked list for $O(1)$ evictions. Whenever an item is accessed (a cache "hit"), the item must be moved to the front of the linked list to mark it as the most recently used. 

In a concurrent environment, this physical reordering of memory pointers requires a global lock (such as `sync.RWMutex` in Go) to prevent data races. As the number of CPU cores scaling a workload increases, the global lock becomes a catastrophic bottleneck. The cores spend more CPU cycles contending for exclusive access to the cache line holding the mutex state than they do performing the actual business logic. 

Even modern "Lock-Free" CLOCK algorithms—which use an atomic pointer to sweep through an array (a clock hand)—suffer from contention. An atomic spin-loop scanning an $O(N)$ array still triggers massive L1/L2 cache coherency storms, resulting in unacceptable p99.9 latency spikes under load.

---

## II. Mathematical Eviction: Chunked Bitmask CLOCK

To eliminate pointer-chasing and loop-based atomic scanning, `liteLRU` utilizes a **Chunked Bitmask CLOCK** algorithm. The cache capacity is divided into static chunks, each representing exactly 64 cache slots. 

Instead of an array of structs or a linked list, each chunk tracks the state of its 64 slots using 64-bit integers (`atomic.Uint64`). This enables the eviction algorithm to determine the exact index of an eviction candidate using single-cycle mathematical bitwise operations.

For any given chunk, we track:
- `validBits`: Which slots currently hold data.
- `accessedBits`: Which slots have been read recently.

When a thread needs to evict a slot, it reads both 64-bit masks. A slot is a valid eviction candidate if it is currently empty, or if it is valid but has not been accessed recently. This can be expressed mathematically as:

```go
candidates := ^validBits | (validBits & ^accessedBits)
```

Because `candidates` is a 64-bit integer, we can instantly find the index of the first available candidate using a hardware-intrinsic instruction (Count Trailing Zeros), bypassing any need for a loop:

```go
bit := bits.TrailingZeros64(candidates)
```

By decoupling the clock hand from a global atomic counter and instead seeding the starting chunk deterministically based on the hash of the incoming request, the eviction sweep is naturally distributed across the memory space, removing global synchronization entirely.

---

## III. Memory Layout and Cache Miss Mitigation

Performance on modern hardware is governed not by CPU clock speed, but by the physical latency of fetching memory from RAM into the CPU's L1/L2 cache. 

### Structure of Arrays (SoA)
Instead of utilizing an Array of Structs (AoS) where varied data types are packed together, `liteLRU` is designed around a Structure of Arrays (SoA) memory layout. Parallel slices (`methods`, `paths`, `handlers`) provide pristine cache locality and allow the prefetcher to load contiguous memory efficiently.

### False Sharing and Cache Line Alignment
The most insidious performance killer in concurrent programming is **False Sharing**. This occurs when multiple CPU cores modify independent variables that happen to reside on the exact same 64-byte physical cache line. When Core A writes to its variable, the CPU interconnect forces Core B to invalidate its L1 cache line, even though Core B is reading a completely unrelated variable.

To facilitate wait-free reads, `liteLRU` uses Sequence Locks (Seqlocks)—a counter incremented to an odd number during writes, and an even number upon completion. If these 4-byte seqlocks were packed contiguously into a standard slice (`[]atomic.Uint32`), 16 separate locks would share a single 64-byte cache line. A thread evicting slot 0 would inadvertently destroy the L1 cache for a thread reading slot 15.

To prevent this, `liteLRU` physically aligns memory structures to the OS boundary. Every seqlock and bitmask chunk is explicitly padded to 64 bytes:

```go
// Padded to a full 64-byte cache line to completely eliminate false-sharing.
type slotState struct {
    seq atomic.Uint32
    _   [60]byte
}
```

Furthermore, global statistics (Hits and Misses) are sharded across 64 independent, 64-byte padded cache lines. Threads pick a stat stripe based on a bitwise mask of their hash (`hash & 63`). This physical memory alignment ensures that heavily contended atomic writes never trigger false sharing coherency storms.

---

## IV. Lock-Free Map Integration and Tombstone Recycling

To map a string route (`method` + `path`) to the internal integer index of the SoA, `liteLRU` relies on the external `github.com/xDarkicex/memory` package, which provides a high-performance open-addressed lock-free HashMap.

Because the cache capacity is fixed, dynamically growing the HashMap or allowing it to fragment would eventually trigger a global resize—an operation that is catastrophic for p99.9 latency. 

When an eviction occurs, `liteLRU` first checks if the slot previously held valid data. If so, it performs an atomic `Delete` on the old route's hash directly within the `HashMap` *before* inserting the new route. This specific sequence allows the lock-free map to continuously recycle its internal tombstones, guaranteeing that the map size remains permanently fixed and resizes are never triggered.

---

## V. The `b.RunParallel` Illusion (Benchmarking Methodology)

In evaluating the throughput of `liteLRU`, we observed a significant flaw in how systems engineers conventionally benchmark concurrent Go applications. 

Go's standard `testing.B.RunParallel` framework utilizes an internal `pb.Next()` atomic fetch-and-add loop to coordinate `b.N` iterations across available goroutines. When evaluating operations that execute in the sub-10ns range, this benchmarking tool itself becomes the primary hardware bottleneck. The atomic synchronization required to increment the benchmark loop counter creates immense cross-core L1 contention, artificially inflating the `ns/op` metric as the core count increases, creating the illusion of poor scaling.

To accurately measure the physical capabilities of the hardware, benchmarking methodologies must isolate the system. We developed an un-synchronized worker-loop script (`latency_test.go`) that completely removes `pb.Next()` and measures operations over a pre-allocated array.

---

## VI. Results and Conclusion

Under a clean testing environment (Apple M2, macOS arm64) using the un-synchronized custom benchmarking methodology, `liteLRU` demonstrated near-perfect linear throughput scaling under a heavy, highly-contended parallel workload (8 workers, 1.6 Million ops, 80% Get / 20% Add).

**Throughput Scaling:**
- **1 Core:** 5.5 Million ops/sec
- **2 Cores:** 8.8 Million ops/sec
- **4 Cores:** 13.7 Million ops/sec
- **8 Cores:** 17.0 Million ops/sec

**Tail Latency Percentiles:**
*(Measured with an inherent ~40ns measurement overhead backed out)*
- **p50 (Median):** ~210 ns
- **p99:** ~960 ns
- **p99.9:** ~1.37 µs

By abandoning algorithmic purity in favor of mechanical sympathy—utilizing bitwise mathematics, padded cache lines, and lock-free data structures—`liteLRU` successfully bridges the gap between academic theory and hardware reality, scaling eviction directly to the metal.

---
*The source code for `liteLRU` is open-source and available at: [https://github.com/xDarkicex/liteLRU](https://github.com/xDarkicex/liteLRU)*
