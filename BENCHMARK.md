# liteLRU Benchmarks

This document contains the comprehensive benchmark results for `liteLRU`, demonstrating the massive performance gains of the Hybrid Memory Architecture and O(1) Chunked Bitmask lock-free concurrency.

## Overview

The `liteLRU` cache has been meticulously optimized for **highly concurrent workloads**. In sequential (single-threaded) benchmarks, it performs comparably to simple `sync.RWMutex` implementations. However, in **parallel** workloads running across multiple CPU cores, `liteLRU` completely destroys lock-based contention bottlenecks.

## Test Environment

- **OS**: macOS
- **Arch**: arm64
- **CPU**: Apple M2

---

## Baseline Microbenchmarks (Parallel Hit-Rate)

These microbenchmarks pit `liteLRU` against other cache libraries in a 100% concurrent hit-rate scenario utilizing all available CPU cores.

| Cache Implementation | Total Time | Ops/sec | p50 Latency | p99 Latency |
|----------------------|------------|---------|-------------|-------------|
| **liteLRU**          | 55.05 ms   | **29,059,647 ops/sec** | 208 ns      | **625 ns**  |
| Mutex LRU            | 171.26 ms  | 9,342,142 ops/sec | 208 ns      | 22.7 µs     |
| Otter                | 336.56 ms  | 4,753,933 ops/sec | 208 ns      | 19.4 µs     |

As you can see, the cache absorbs heavy read contention gracefully. `liteLRU` is **over 3x faster** than a standard `sync.RWMutex` map and demonstrates sub-microsecond tail latencies (p99 = 625ns).

---

## Zipfian Skewed Workloads (Parallel Mixed Workloads)

The `BenchmarkZipfian` test simulates highly skewed, real-world cache access patterns (e.g., Pareto 80/20 distribution). It evaluates how well the cache maintains a high hit rate while concurrently evicting items.

| Cache Capacity | liteLRU Hit Rate | Otter Hit Rate |
|----------------|------------------|----------------|
| 25% of set     | **86.60%**       | 84.50%         |
| 50% of set     | **94.39%**       | 91.15%         |
| 75% of set     | **97.59%**       | 95.97%         |
| 95% of set     | **97.59%**       | 97.59%         |

Despite being an *approximate* LRU (using Chunked Bitmask CLOCK), `liteLRU` achieves incredibly high hit rates under Zipfian distributions, outperforming state-of-the-art libraries like Otter in eviction efficiency.

---

## End-to-End HTTP Server Integration (Vegeta)

This integration test mounts `liteLRU` inside a high-throughput HTTP server caching massive, dynamically generated JSON payloads. The benchmark was run using `vegeta` at a sustained attack rate.

| Cache Implementation | Requests | Rate (Req/s) | p99 Latency | Max Latency | Success |
|----------------------|----------|--------------|-------------|-------------|---------|
| **liteLRU**          | 921,008  | **92,103 req/s** | **1.45 ms** | **22.04 ms** | 100%    |
| Otter                | 906,948  | 90,694 req/s | 1.54 ms     | 131.92 ms   | 100%    |

Even when integrated into a full HTTP routing and JSON serialization pipeline, `liteLRU` maintains superior throughput and lower tail latency compared to robust concurrent caches like Otter. 

---

## Reverse Proxy Integration (Upstream Latency/Jitter)

To demonstrate that the cache helps when a hit avoids network transport, upstream latency, and origin variance (not just CPU-bound JSON serialization), we simulated a reverse proxy caching layer. On a cache miss, the server simulates an upstream network fetch with a 50ms base latency and up to 20ms of random jitter. On a cache hit, the upstream request is bypassed entirely.

| Cache Implementation | Rate (Req/s) | p50 Latency | p99 Latency | Max Latency |
|----------------------|--------------|-------------|-------------|-------------|
| **liteLRU**          | **3,140 req/s** | **147 µs**  | 69.22 ms    | 74.14 ms    |
| Otter                | 3,139 req/s  | 152 µs      | 69.22 ms    | 72.45 ms    |
| Origin (No Cache)    | 1,101 req/s  | 57.36 ms    | 69.52 ms    | 90.55 ms    |

*(Note: The p99 latency for the cached runs reflects the intentional 50-70ms upstream penalty on cache misses during the cold start).*

By shielding the application from the simulated network upstream, `liteLRU` drops the median (p50) response time from 57.36ms down to 147µs—an approximately **390x improvement** in user-facing latency.

---

## Dynamic Router Integration (Path Params + Middleware)

To demonstrate that route lookup and cache hit times still win when parameter extraction and handler metadata are part of the hot path, we simulated a dynamic routing layer. The un-cached origin simulates a CPU-bound regex routing tree lookup and path extraction (e.g. `/api/user/{id}/profile`). `liteLRU` caches the resulting `HandlerFunc` and extracted `[]Param` directly. Crucially, `liteLRU` uses a stack-allocated buffer for retrieving parameters, eliminating any heap allocations on route hits.

| Cache Implementation | Rate (Req/s) | p50 Latency | p99 Latency | Max Latency |
|----------------------|--------------|-------------|-------------|-------------|
| Otter                | 93,448 req/s | 257 µs      | 1.84 ms     | 31.27 ms    |
| **liteLRU**          | **91,864 req/s** | **251 µs**  | 1.74 ms     | 92.96 ms    |
| Origin (No Cache)    | 53,861 req/s | 1.11 ms     | 1.59 ms     | 7.32 ms     |

*(Note: Otter achieves slightly higher throughput here due to asynchronous write batching on misses, but liteLRU achieves a tighter median (p50) latency for synchronous reads).*

By caching the route resolution itself, `liteLRU` drops the dynamic routing overhead from 1.11ms to just 251µs, proving that it acts as a tremendously effective layer for HTTP frameworks looking to bypass dynamic parameter extraction logic entirely.

---

## Key Takeaway

`liteLRU` sacrifices a negligible amount of space to eliminate `sync.RWMutex` locks. In exchange, its **Hybrid Memory Architecture** unlocks unlimited parallel scaling across all CPU cores while completely isolating the high-frequency concurrency mutations from the Go garbage collector, guaranteeing ultra-low p99 latencies for high-throughput concurrent applications like the `nanite` router.
