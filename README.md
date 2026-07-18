# liteLRU

Hey there! 👋 I'm super excited to introduce `liteLRU` – an absurdly fast, **100% lock-free**, memory-efficient LRU cache for Go that'll make your apps fly. I originally built this for the [nanite router](https://github.com/xDarkicex/nanite) to bypass Go's garbage collector on hot paths, but the architecture was just too good to keep to myself. 

Ready to supercharge your caching?

[![Go Reference](https://pkg.go.dev/badge/github.com/xDarkicex/liteLRU)](https://pkg.go.dev/github.com/xDarkicex/liteLRU)
[![Go Report Card](https://goreportcard.com/badge/github.com/xDarkicex/liteLRU)](https://goreportcard.com/report/github.com/xDarkicex/liteLRU)

## What Makes This Cache Special?

Most Go caches wrap a map in a mutex and call it a day, or use background goroutines that spike your tail latency. We took a bottom-up systems approach:

- **Hybrid Memory Architecture (Zero GC Pressure)**: Heavily mutated synchronization state is allocated off-heap via anonymous `mmap`. This completely isolates cache-line interconnect traffic from Go's garbage collector, while safely tracking your Go pointers on the standard heap.
- **100% Bounded Lock-Free**: Absolutely zero `sync.Mutex` or `sync.RWMutex`. Reads are bounded-probe lock-free, and writes are contention-bounded via a load-shedding admission protocol.
- **O(1) Bitmask Eviction**: We use mathematical Chunked Bitmask CLOCK algorithms (powered by hardware `CTZ` / `bits.TrailingZeros64`) to find eviction victims in a single CPU cycle. No more O(N) list traversal.
- **Hardware-Inspired Routing**: Uses a **64-way set associative SWAR architecture** for direct lock-free routing. Zero tombstones, zero table compactions.
- **Cache-Line Padded Seqlocks**: Memory layouts are explicitly padded to your target platform's coherence-line stride (e.g., 64 bytes on x86_64, 128 bytes on Apple Silicon) to mathematically eliminate false-sharing CPU cache bounces.

## Getting Started

```bash
go get github.com/xDarkicex/liteLRU
```

## Let's Write Some Code!

Here's how easy it is to use:

```go
package main

import (
    "fmt"
    "github.com/xDarkicex/liteLRU"
)

func main() {
    // Create a sweet cache with 1024 entries and 10 params per entry max
    cache := liteLRU.NewLRUCache(1024, 10)
    
    // Your handler function - could be anything!
    handler := func() { fmt.Println("Hello from Silicon Valley!") }
    
    // Add something to the cache
    cache.Add("GET", "/users/123", handler, []liteLRU.Param{
        {Key: "id", Value: "123"},
        {Key: "format", Value: "json"},
    })
    
    // Later, grab it back lightning-fast (Zero allocations!)
    if h, params, found := cache.Get("GET", "/users/123"); found {
        h() // Prints our message
        fmt.Printf("Check out these params: %+v\n", params)
    }
    
    // How's our cache performing?
    hits, misses, _, ratio := cache.Stats()
    fmt.Printf("Hit ratio: %.2f%% - Not bad!\n", ratio*100)
}
```

## The API is Super Clean

Think of this as your cheat sheet:

### The Building Blocks

```go
type Param struct {
    Key   string
    Value string
}

type HandlerFunc func()
```

### Creating Your Cache

```go
// This automatically rounds capacity up to a power-of-two!
cache := liteLRU.NewLRUCache(capacity, maxParams)
```

### The Methods You'll Love

```go
// Store something awesome (Lock-free, contention-bounded)
cache.Add(method, path string, handler HandlerFunc, params []Param)

// Grab it back in nanoseconds (Zero-allocation, lock-free)
handler, params, found := cache.Get(method, path string)

// Spring cleaning
cache.Clear()

// Check the padded stat stripes
hits, misses, _, ratio := cache.Stats()
```

## The Numbers Will Blow Your Mind

We benchmarked `liteLRU` heavily against a synthetic write-heavy Zipfian workload. It sustains **~30,000,000 ops/sec** under a 50/50 Get/Add load by dynamically shedding pathological admissions.

See the [BENCHMARK.md](BENCHMARK.md) file for the full detailed output of our parallel and sequential benchmarks!

Here is a quick teaser of our `RunParallel` performance running across multiple cores:

| Cores | Workload (80% Get / 20% Add) | Speed (ns/op) | Allocations |
| --- | --- | --- | --- |
| 1 | ParallelMixedWorkload | **26.13 ns** | 0 |
| 2 | ParallelMixedWorkload-2 | **19.54 ns** | 0 |
| 4 | ParallelMixedWorkload-4 | **30.17 ns** | 0 |
| 8 | ParallelMixedWorkload-8 | **46.95 ns** | 0 |

*(Yes, that is ~30ns per operation for a fully thread-safe, LRU-evicting cache!)*

### Concurrent Latency Percentiles (Clean Environment)

We also run a heavy concurrent latency test (8 workers, 1.6M ops). *(Note: Because this test wraps every single operation in `time.Now()` and `time.Since()`, there is an inherent ~30-50ns measurement overhead added to every op).*

**Estimated True Latency (Overhead Removed):**

* **p50 (Median)**: ~210 ns
* **p99**: ~960 ns
* **p99.9**: **~1.37 µs**

Check out [BENCHMARK.md](BENCHMARK.md) for the full raw metrics and a deep dive into why Go's `b.RunParallel` obscures true scaling.

## So How Does It Work?

Here's where things get interesting. We threw out standard cache designs and built this from the hardware up:

### 1. Hybrid Memory & Structure of Arrays (SoA)

Instead of using standard pointer-based struct slices, `liteLRU` organizes its data into contiguous, parallel arrays (`methods`, `paths`, `handlers`, `params`). More importantly, all fast-changing metadata is mapped completely **off-heap**. This gives you pristine CPU cache locality while blinding the Go GC to your high-frequency state changes.

### 2. O(1) Chunked Bitmask CLOCK Eviction

Traditional LRU caches use doubly-linked lists. Even lock-free CLOCK implementations use an O(N) atomic scan loop which causes massive p99.9 latency spikes under contention.

We chunk the cache into blocks of 64 slots. Each chunk is represented by a single `atomic.Uint64` bitmask. Finding an eviction victim mathematically requires zero loops:

```go
// Mathematical bitwise O(1) eviction
candidates := ^validBits | (validBits & ^accessedBits)
bit := bits.TrailingZeros64(candidates) 
```

### 3. 64-Way Set Associative SWAR Routing

We mathematically eliminated the Hash Map (and the tombstone compactions that bottleneck concurrency). Instead, `liteLRU` groups slots into 64-way associative sets, just like hardware L1 CPU caches. We use a single `uint64` word containing eight 1-byte hash signatures and **SIMD Within A Register (SWAR)** to instantly scan 8 slots per CPU bitwise instruction.

### 4. False-Sharing Immune Seqlocks

To keep reads completely lock-free without tearing data, every slot maintains a sequence lock (an `atomic.Uint32`). If an `Add` concurrently evicts and overwrites a slot while a `Get` is reading it, the `Get` detects the sequence change and safely reports a cache miss. Because these locks are strictly padded to your CPU's exact coherence-line size, they are mathematically immune to false-sharing performance collapse.

## Real-World Use Cases

### Web Routers (like nanite)

```go
// Cache those route handlers for lightning-fast routing
routerCache := liteLRU.NewLRUCache(1024, 10)
routerCache.Add("GET", "/api/users", usersHandler, []liteLRU.Param{})

// When a request comes in - boom, instant response!
if handler, params, found := routerCache.Get("GET", "/api/users"); found {
    handler(ctx, params)
}
```

### Database Query Caching

```go
// Stop hammering your database for the same data
queryCache := liteLRU.NewLRUCache(512, 5)

// Cache those expensive query results
results := executeExpensiveQuery(userId)
params := resultsToParams(results)
queryCache.Add("user:"+userId, "query:details", func() {}, params)

// Later, when you need them again
if _, params, found := queryCache.Get("user:"+userId, "query:details"); found {
    // Look ma, no database call!
    displayUserDetails(paramsToResults(params))
}
```

## Run Your Own Benchmarks

Want to see how amazing this performs on your own hardware? I've included comprehensive benchmarking tools:

```bash
# Run all parallel and sequential benchmarks across multiple cores
go test -bench=. -benchmem -cpu=1,2,4,8
```

## Want to Contribute?

I'd love your help making liteLRU even better! Here are some ways you can contribute:

* Find a bug? Open an issue!
* Have an idea for an optimization? Let's hear it!
* Want to improve the docs or add examples? Amazing!
* Got a cool use case? I'd love to see it!

Just follow standard Go formatting and testing practices, and we're golden.

## License

MIT License - see LICENSE file for details. Use it anywhere you want!

---

## Let's Supercharge Your Go Apps!

If you're tired of slow caches that eat memory for breakfast and bottleneck on locks, give liteLRU a try. Your users (and your ops team) will thank you! Drop a star on GitHub if you find it useful, and feel free to reach out with any questions.

Happy caching! 🚀

---

## Academic References

The design of `liteLRU` is grounded in the following computer science literature. See [WHITEPAPER.md](WHITEPAPER.md) for the full technical derivation.

[1] Papamarcos, M. S. and Patel, J. H. "A Low-Overhead Coherence Solution for Multiprocessors with Private Cache Memories." *ACM SIGARCH*, 1984. https://doi.org/10.1145/773453.808204
[2] Amdahl, G. M. "Validity of the Single Processor Approach to Achieving Large Scale Computing Capabilities." *AFIPS*, 1967. https://doi.org/10.1145/1465482.1465560
[3] Corbató, F. J. "A Paging Experiment with the Multics System." *MIT Press*, 1969. *(origin of the CLOCK algorithm)*
[4] Belady, L. A. "A Study of Replacement Algorithms for a Virtual-Storage Computer." *IBM Systems Journal*, 1966. https://doi.org/10.1147/sj.52.0078
[5] Yang, J. et al. "FIFO Queues are All You Need for Cache Eviction." *SOSP '23*, ACM, 2023. https://doi.org/10.1145/3600006.3613147
[6] Zhang, Y. et al. "SIEVE is Simpler than LRU." *USENIX NSDI*, 2024. https://www.usenix.org/conference/nsdi24/presentation/zhang-yazhuo
[7] Bolosky, W. J. and Scott, M. L. "False Sharing and Its Effect on Shared Memory Performance." *USENIX*, 1993. https://dl.acm.org/doi/10.5555/1295415.1295418
[8] Herlihy, M. and Shavit, N. "The Art of Multiprocessor Programming." *ACM PODC*, 2004. https://doi.org/10.1145/1011767.1011768
[9] Lameter, C. "Effective Synchronization on Linux/NUMA Systems." *Gelato Conference*, 2005. https://lameter.com/gelato2005.pdf
[10] Hudson, R. L. "Go GC: Prioritizing Low Latency and Simplicity." Go Blog, 2015. https://go.dev/blog/ismmkeynote
[11] Intel Corporation. "Intel 64 and IA-32 Architectures Software Developer's Manual, TZCNT Instruction." 2023. https://www.intel.com/content/www/us/en/developer/articles/technical/intel-sdm.html
[12] ARM Limited. "ARM Architecture Reference Manual: CLZ Instruction." DDI 0487, 2023. https://developer.arm.com/documentation/ddi0487/latest/
