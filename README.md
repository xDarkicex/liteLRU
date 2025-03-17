# liteLRU

A lightning-fast, memory-efficient LRU cache for Go applications. Originally developed for the [nanite router](https://github.com/xDarkicex/nanite), now available as a standalone package to supercharge your caching needs.

[![Go Reference](https://pkg.go.dev/badge/github.com/xDarkicex/liteLRU)](https://pkg.go.dev/github.com/xDarkicex/liteLRU)
[![Go Report Card](https://goreportcard.com/badge/github.com/xDarkicex/liteLRU)](https://goreportcard.com/report/github.com/xDarkicex/liteLRU)

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

## API Reference

### Types

```go
type Param struct {
    Key   string
    Value string
}

type HandlerFunc func()
```

### Creating a Cache

```go
// Create a new cache with the specified capacity and maximum parameters per entry
// Capacity will be rounded up to the nearest power of two for optimal performance
cache := liteLRU.NewLRUCache(capacity, maxParams)
```

### Methods

```go
// Add an entry to the cache or update an existing one
cache.Add(method, path string, handler HandlerFunc, params []Param)

// Retrieve an entry from the cache
handler, params, found := cache.Get(method, path string)

// Clear all entries and reset statistics
cache.Clear()

// Get cache statistics (hits, misses, hit ratio)
hits, misses, ratio := cache.Stats()
```

## Performance Benchmarks

liteLRU has been extensively benchmarked across various configurations. Here are the results (measured on Apple M1):

### Get Operations

| Cache Size | Scenario                 | Operations/sec | Time (ns/op) | Memory (B/op) | Allocations |
|------------|--------------------------|---------------:|-------------:|--------------:|------------:|
| 128        | HighHitRatio_FewParams   | 13,721,322     | 87.58        | 0             | 0           |
| 128        | HighHitRatio_ManyParams  | 14,732,052     | 79.72        | 0             | 0           |
| 128        | LowHitRatio_FewParams    | 14,984,952     | 79.19        | 0             | 0           |
| 1024       | HighHitRatio_FewParams   | 12,106,135     | 98.43        | 0             | 0           |
| 4096       | HighHitRatio_FewParams   | 10,648,020     | 112.1        | 0             | 0           |

### Add Operations

| Cache Size | Scenario                 | Operations/sec | Time (ns/op) | Memory (B/op) | Allocations |
|------------|--------------------------|---------------:|-------------:|--------------:|------------:|
| 128        | HighHitRatio_FewParams   | 3,283,052      | 347.7        | 161           | 7           |
| 128        | HighHitRatio_ManyParams  | 1,596,214      | 751.6        | 592           | 25          |
| 1024       | HighHitRatio_FewParams   | 3,087,039      | 385.3        | 162           | 7           |
| 4096       | HighHitRatio_FewParams   | 2,837,850      | 423.4        | 162           | 7           |

### Mixed Operations (75% Get, 25% Add)

| Cache Size | Scenario                 | Operations/sec | Time (ns/op) | Memory (B/op) | Allocations |
|------------|--------------------------|---------------:|-------------:|--------------:|------------:|
| 128        | HighHitRatio_FewParams   | 7,084,918      | 167.8        | 42            | 2           |
| 1024       | HighHitRatio_FewParams   | 6,525,136      | 181.3        | 42            | 1           |
| 4096       | HighHitRatio_FewParams   | 5,907,231      | 203.6        | 43            | 1           |

### Real-World Workload

| Benchmark              | Operations/sec | Time (ns/op) | Memory (B/op) | Allocations |
|------------------------|---------------:|-------------:|--------------:|------------:|
| RealWorldWorkload      | 8,813,767      | 143.6        | 6             | 0           |

These benchmarks demonstrate liteLRU's exceptional performance characteristics: Get operations complete in under 100 nanoseconds with zero allocations, even for large cache sizes.

## How It Works

liteLRU combines several advanced techniques to achieve its high performance:

### 1. Array-Based Doubly-Linked List

Unlike traditional LRU caches that use pointers for linked lists, liteLRU uses an array-based implementation where nodes reference each other by index:

```
┌─────────┐     ┌─────────┐     ┌─────────┐
│ Entry 0 │◄───►│ Entry 1 │◄───►│ Entry 2 │◄──┐
└─────────┘     └─────────┘     └─────────┘   │
     ▲                                         │
     │                                         │
     └─────────────────────────────────────────┘
```

This approach:
- Improves cache locality
- Reduces pointer chasing
- Minimizes memory fragmentation

### 2. Parameter Pooling

liteLRU uses a tiered parameter slice pooling system that dramatically reduces GC pressure:

```go
var paramSlicePools = [5]sync.Pool{
    {New: func() interface{} { return make([]Param, 0, 4) }},   // For 1-4 params
    {New: func() interface{} { return make([]Param, 0, 8) }},   // For 5-8 params
    {New: func() interface{} { return make([]Param, 0, 16) }},  // For 9-16 params
    {New: func() interface{} { return make([]Param, 0, 32) }},  // For 17-32 params
    {New: func() interface{} { return make([]Param, 0, 64) }},  // For 33-64 params
}
```

This approach recycles parameter slices based on their capacity, significantly reducing memory allocations.

### 3. String Interning

To reduce memory usage further, liteLRU implements string interning for HTTP methods and paths:

```go
// Only one copy of each unique string is stored
method = internString(method)
path = internString(path)
```

This is particularly effective for HTTP methods, which are limited to a small set of values.

### 4. Power-of-Two Sizing

The cache size is automatically rounded up to the nearest power of two, which optimizes for:
- Hash table efficiency
- Modulo operations in the doubly-linked list
- Memory allocation patterns

### 5. Lock Optimization

liteLRU uses a read-write mutex, allowing multiple simultaneous reads:
- Read locks for `Get` operations until promotion is needed
- Write locks only when modifying the LRU ordering

## Use Cases

### Web Routers (like nanite)

```go
// Cache route handlers for blazing fast routing
routerCache := liteLRU.NewLRUCache(1024, 10)
routerCache.Add("GET", "/api/users", usersHandler, []liteLRU.Param{})

// Later, during request handling
if handler, params, found := routerCache.Get("GET", "/api/users"); found {
    handler(ctx, params)
}
```

### Database Query Results

```go
// Cache expensive database query results
queryCache := liteLRU.NewLRUCache(512, 5)

// When executing a query
results := executeExpensiveQuery(userId)
params := resultsToParams(results)
queryCache.Add("user:"+userId, "query:details", func() {
    // Optional refresh handler
}, params)

// Later, when retrieving results
if _, params, found := queryCache.Get("user:"+userId, "query:details"); found {
    // Use cached results from params
    displayUserDetails(paramsToResults(params))
} else {
    // Cache miss - execute query again
}
```

### Template Rendering

```go
// Cache rendered templates
templateCache := liteLRU.NewLRUCache(256, 3)

// After rendering a template
renderedHTML := renderTemplate("homepage", data)
templateCache.Add("homepage", "variant:mobile", func() {
    // Optional re-render handler
}, []liteLRU.Param{
    {Key: "html", Value: renderedHTML},
})

// Later, when serving the page
if _, params, found := templateCache.Get("homepage", "variant:mobile"); found {
    for _, param := range params {
        if param.Key == "html" {
            serveHTML(param.Value)
            break
        }
    }
}
```

## Integration with nanite Router

liteLRU was originally developed for the [nanite router](https://github.com/xDarkicex/nanite), where it significantly accelerates route matching. Here's how nanite integrates with liteLRU:

```go
// Inside nanite router code
cache := liteLRU.NewLRUCache(1024, 10)

// During request handling
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
    method := req.Method
    path := req.URL.Path
    ctx := newContext(w, req)
    
    // Fast path: try the cache first
    if handler, params, found := cache.Get(method, path); found {
        ctx.Params = params
        handler(ctx)
        return
    }
    
    // Slow path: resolve route manually
    handler, params := r.findRoute(method, path)
    if handler != nil {
        // Cache for next time
        cache.Add(method, path, func() { handler(ctx) }, params)
        ctx.Params = params
        handler(ctx)
    } else {
        r.handleNotFound(ctx)
    }
}
```

This integration provides:
- Near-instant response for repeated routes
- Significant reduction in CPU usage for path matching
- Lower memory usage through parameter recycling

## Benchmark Your Use Case

liteLRU includes comprehensive benchmarking tools to help you evaluate performance for your specific workload:

```bash
# Run all benchmarks
go test -bench=. -benchmem

# Run specific benchmark pattern
go test -bench=BenchmarkLRUCache/Get -benchmem

# Benchmark with specific cache size
go test -bench=Size128 -benchmem

# Run real-world workload benchmark
go test -bench=RealWorldWorkload -benchmem
```

## Implementation Details

liteLRU is implemented with approximately 300 lines of highly optimized Go code. Key implementation details:

- **Fixed-size array** for cache entries to avoid slice resizing
- **Pre-allocated map** with 2x capacity to reduce hash collisions
- **Circular doubly-linked list** for efficient LRU operations
- **Atomic counters** for hit/miss statistics to avoid lock contention
- **Panic recovery** in critical methods for maximum robustness

## Contributing

Contributions are welcome! Here are some ways you can contribute:

- Report bugs and request features by creating issues
- Improve documentation or add examples
- Submit pull requests with bug fixes or enhancements
- Share benchmarks and performance optimizations

Please follow Go's standard formatting and testing practices.

## License

MIT License - see LICENSE file for details.

---

📈 **Boost your application's performance today with liteLRU!**
