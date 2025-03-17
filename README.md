# liteLRU

A lightning-fast, memory-efficient LRU cache for Go applications. Originally developed for the [nanite router](https://github.com/xDarkicex/nanite), now available as a standalone package to supercharge your caching needs.

[![Go Reference](https://pkg.go.dev/badge/github.com/xDarki.go.dev/github.com/xDarkicexd](https://goreportcard.com/badge/github.com/xDarkicextcard.com/report/github.coures

- **Blazing Fast**: Get operations as low as 79ns with zero allocations
- **Memory Efficient**: Smart parameter pooling minimizes GC pressure
- **Thread-Safe**: Concurrent access with minimal lock contention
- **Customizable**: Configure cache sizes and parameter limits
- **Production Ready**: Battle-tested in the nanite router
- **Optimized Design**: Array-based doubly-linked list for O(1) operations

## Installation

```bash
go get github.com/xDarkicex/liteLRU
```

## Quick Start

```go
package main

import (
    "fmt"
    "github.com/xDarkicex/liteLRU"
)

func main() {
    // Create a new cache with capacity 128 and max 10 params per entry
    cache := liteLRU.NewLRUCache(128, 10)
    
    // Define a handler function
    handler := func() { fmt.Println("Hello, world!") }
    
    // Add an entry to the cache
    cache.Add("GET", "/users/123", handler, []liteLRU.Param{
        {Key: "id", Value: "123"},
        {Key: "format", Value: "json"},
    })
    
    // Retrieve from cache
    if h, params, found := cache.Get("GET", "/users/123"); found {
        h() // Prints "Hello, world!"
        fmt.Printf("Parameters: %+v\n", params)
    }
    
    // Check cache statistics
    hits, misses, ratio := cache.Stats()
    fmt.Printf("Hit ratio: %.2f%%\n", ratio*100)
}
```

## Why liteLRU?

Ever built a router that slows to a crawl under load? I have. The nanite router needed a caching solution that could handle thousands of routes with lightning speed. After extensive testing and optimization, liteLRU was born.

What makes it special?

- **String Interning**: Reduces memory usage by storing one copy of each unique string
- **Parameter Pooling**: Pre-allocated parameter slice pools eliminate GC pressure
- **Power-of-Two Sizing**: Automatically optimizes cache size for best performance
- **Minimal Locks**: Read-heavy operations use read locks for high concurrency

## Performance

Let's talk numbers. Here's what liteLRU can do (benchmarked on Apple M1):

```
BenchmarkLRUCache/Get_Size128_HighHitRatio_FewParams-8    13721322     87.58 ns/op     0 B/op     0 allocs/op
BenchmarkLRUCache/Get_Size4096_HighHitRatio_FewParams-8   10648020    112.1 ns/op      0 B/op     0 allocs/op
BenchmarkLRUCache/Add_Size128_HighHitRatio_FewParams-8     3283052    347.7 ns/op    161 B/op     7 allocs/op
BenchmarkParamPooling/RealWorldWorkload-8                  8813767    143.6 ns/op      6 B/op     0 allocs/op
```

That's right - Get operations complete in under 100 nanoseconds with zero allocations. Even our realistic workload benchmark shows extraordinary performance with practically zero allocations.

## Advanced Usage

### Custom Cache Sizes

liteLRU automatically optimizes for power-of-two cache sizes, but you can specify any size:

```go
// Small cache for limited resources
smallCache := liteLRU.NewLRUCache(64, 5)

// Large cache for high-traffic applications
largeCache := liteLRU.NewLRUCache(4096, 20)
```

### Cache Clearing

Need to reset your cache? No problem:

```go
// Clear all entries and reset statistics
cache.Clear()
```

### Performance Monitoring

Monitor cache effectiveness with built-in statistics:

```go
hits, misses, ratio := cache.Stats()
fmt.Printf("Cache performance: %.2f%% hit rate (%d hits, %d misses)\n", 
           ratio*100, hits, misses)
```

## How It Works

liteLRU uses an array-based doubly-linked list for O(1) LRU operations, combined with a map for O(1) lookups:

```
┌─────────┐     ┌─────────┐     ┌─────────┐
│ Entry 0 │◄───►│ Entry 1 │◄───►│ Entry 2 │◄──┐
└─────────┘     └─────────┘     └─────────┘   │
     ▲                                         │
     │                                         │
     └─────────────────────────────────────────┘
```

When a cached item is accessed, it's moved to the front of the list (most recently used). When the cache is full, the item at the end of the list (least recently used) is evicted to make room for new entries.

## Use Cases

### Web Routers (like nanite)

```go
// Cache route handlers for blazing fast routing
routerCache := liteLRU.NewLRUCache(1024, 10)
routerCache.Add("GET", "/api/users", usersHandler, []liteLRU.Param{})

// Later, during request handling
if handler, params, found := routerCache.Get("GET", "/api/users"); found {
    handler()
}
```

### Database Query Results

```go
// Cache expensive database query results
queryCache := liteLRU.NewLRUCache(512, 5)
queryCache.Add("users:active", "query:hash123", func() {
    // This handler can be used to refresh the data if needed
}, resultsAsParams)
```

### Template Rendering

```go
// Cache rendered templates
templateCache := liteLRU.NewLRUCache(256, 3)
templateCache.Add("homepage", "variant:mobile", func() {
    // Re-render if needed
}, []liteLRU.Param{
    {Key: "html", Value: renderedHTML},
})
```

## nanite Integration

liteLRU was extracted from the nanite router, where it significantly improved routing performance. In nanite, it's used to cache route resolution results, turning expensive path matching operations into lightning-fast lookups.

Here's how nanite leverages liteLRU:

```go
// Inside nanite router code
cache := liteLRU.NewLRUCache(1024, 10)

// When handling a request
method := req.Method
path := req.URL.Path

// Try the cache first
if handler, params, found := cache.Get(method, path); found {
    // Fast path: use cached handler and params
    handler(ctx, params)
    return
}

// Slow path: resolve route manually
handler, params := router.findRoute(method, path)
if handler != nil {
    // Cache for next time
    cache.Add(method, path, handler, params)
    handler(ctx, params)
}
```

## Benchmarking Your Use Case

liteLRU includes comprehensive benchmarking tools. To run the benchmarks:

```bash
go test -bench=. -benchmem
```

The test suite covers various cache sizes, hit ratios, and parameter counts to help you understand performance across different scenarios.

## Contributing

Contributions welcome! Feel free to submit issues or pull requests.

## License

MIT License - see LICENSE file for details.

---

📈 **Boost your application's performance today with liteLRU!**
