# liteLRU

Hey there! 👋 I'm super excited to introduce liteLRU – an absurdly fast, memory-efficient LRU cache for Go that'll make your apps fly! I originally built this for the [nanite router](https://github.com/xDarkicex/nanite), but it was just too good to keep to myself. So here it is as a standalone package ready to supercharge your caching needs!

[![Go Reference](https://pkg.go.dev/badge/github.com/xDarkicex/liteLRU)](https://pkg.go.dev/github.com/xDarkicex/liteLRU)
[![Go Report Card](https://goreportcard.com/badge/github.com/xDarkicex/liteLRU)](https://goreportcard.com/report/github.com/xDarkicex/liteLRU)
This Cache Special?

Let's break it down:

- **Mind-blowing Speed**: Get operations clock in at ~79ns with zero allocations. Yes, nanoseconds! ⚡
- **Memory Wizardry**: Smart parameter pooling that'll make the GC practically forget you exist
- **Thread-Safe Magic**: Handle concurrent access without breaking a sweat
- **Totally Customizable**: Dial in your cache size and parameter limits to fit your exact needs
- **Battle-tested**: Already powering the nanite router in production
- **Clever Design**: Array-based doubly-linked list giving you O(1) operations across the board

## Getting Started

First things first:

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
    // Create a sweet cache with 128 entries and 10 params per entry max
    cache := liteLRU.NewLRUCache(128, 10)
    
    // Your handler function - could be anything!
    handler := func() { fmt.Println("Hello from Silicon Valley!") }
    
    // Add something to the cache
    cache.Add("GET", "/users/123", handler, []liteLRU.Param{
        {Key: "id", Value: "123"},
        {Key: "format", Value: "json"},
    })
    
    // Later, grab it back lightning-fast
    if h, params, found := cache.Get("GET", "/users/123"); found {
        h() // Prints our message
        fmt.Printf("Check out these params: %+v\n", params)
    }
    
    // How's our cache performing?
    hits, misses, ratio := cache.Stats()
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
// This automatically rounds capacity up to power-of-two - clever stuff!
cache := liteLRU.NewLRUCache(capacity, maxParams)
```

### The Methods You'll Love

```go
// Store something awesome
cache.Add(method, path string, handler HandlerFunc, params []Param)

// Grab it back in nanoseconds
handler, params, found := cache.Get(method, path string)

// Spring cleaning
cache.Clear()

// How's it performing?
hits, misses, ratio := cache.Stats()
```

## The Numbers Will Blow Your Mind

I'm a bit of a performance junkie, so I've benchmarked liteLRU across tons of different configurations. Here's what my M1 Mac produced:

### Get Operations

| Cache Size | Scenario                 | Operations/sec | Time (ns/op) | Memory (B/op) | Allocations |
|------------|--------------------------|---------------:|-------------:|--------------:|------------:|
| 128        | HighHitRatio_FewParams   | 13,721,322     | 87.58        | 0             | 0           |
| 128        | HighHitRatio_ManyParams  | 14,732,052     | 79.72        | 0             | 0           |
| 4096       | HighHitRatio_FewParams   | 10,648,020     | 112.1        | 0             | 0           |

Look at those zeros in the allocation columns! That's the beauty of careful memory management. Almost 15 million operations per second with zero allocations? That's what I call a win!

### Add Operations

| Cache Size | Scenario                 | Operations/sec | Time (ns/op) | Memory (B/op) | Allocations |
|------------|--------------------------|---------------:|-------------:|--------------:|------------:|
| 128        | HighHitRatio_FewParams   | 3,283,052      | 347.7        | 161           | 7           |
| 128        | HighHitRatio_ManyParams  | 1,596,214      | 751.6        | 592           | 25          |
| 4096       | HighHitRatio_FewParams   | 2,837,850      | 423.4        | 162           | 7           |

Add operations are naturally a bit more expensive (we're allocating memory after all), but still crazy fast at over 3 million ops/sec in the best case!

### Real-World Workload

| Benchmark              | Operations/sec | Time (ns/op) | Memory (B/op) | Allocations |
|------------------------|---------------:|-------------:|--------------:|------------:|
| RealWorldWorkload      | 8,813,767      | 143.6        | 6             | 0           |

This is the one I'm most proud of! Almost 9 million ops/sec with practically zero allocations in a realistic mixed workload. That's what happens when you optimize for the real world!

## So How Does It Work?

Here's where things get interesting! Let me walk you through the secret sauce:

### 1. Array-Based Linked List Magic

Instead of using pointer-based linked lists (so 2010!), I've implemented an array-based doubly-linked list where nodes reference each other by index:

```
┌─────────┐     ┌─────────┐     ┌─────────┐
│ Entry 0 │◄───►│ Entry 1 │◄───►│ Entry 2 │◄──┐
└─────────┘     └─────────┘     └─────────┘   │
     ▲                                         │
     │                                         │
     └─────────────────────────────────────────┘
```

This is amazing for cache locality, reduces pointer chasing, and keeps memory nice and tidy.

### 2. Parameter Pooling That Actually Works

Check this out:

```go
var paramSlicePools = [5]sync.Pool{
    {New: func() interface{} { return make([]Param, 0, 4) }},   // For smaller routes
    {New: func() interface{} { return make([]Param, 0, 8) }},   // Medium routes
    {New: func() interface{} { return make([]Param, 0, 16) }},  // Larger routes
    {New: func() interface{} { return make([]Param, 0, 32) }},  // API-heavy routes
    {New: func() interface{} { return make([]Param, 0, 64) }},  // The kitchen sink
}
```

By recycling parameter slices based on their capacity, we avoid tons of allocations. The GC barely notices we're here!

### 3. String Interning (Sounds Fancy, Actually Simple)

For HTTP methods and paths, we use string interning:

```go
// Store just one copy of each unique string
method = internString(method)
path = internString(path)
```

This is particularly awesome for HTTP methods, since there are only like 9 of them in common use.

### 4. Power-of-Two Sizing (Small Detail, Big Impact)

We automatically round your cache size up to the nearest power of two. Why? It makes hash tables more efficient, speeds up modulo operations, and generally makes the computer happier. Trust me on this one!

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

## How We Use It in nanite Router

The nanite router is where liteLRU was born, and it's a perfect example of how this cache can transform performance:

```go
// Inside the router code
cache := liteLRU.NewLRUCache(1024, 10)

// During request handling
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
    method := req.Method
    path := req.URL.Path
    ctx := newContext(w, req)
    
    // Try the fast path first
    if handler, params, found := cache.Get(method, path); found {
        // Cache hit! Skip all the router matching logic
        ctx.Params = params
        handler(ctx)
        return
    }
    
    // Cache miss - do it the slow way once
    handler, params := r.findRoute(method, path)
    if handler != nil {
        // But cache it for next time!
        cache.Add(method, path, func() { handler(ctx) }, params)
        ctx.Params = params
        handler(ctx)
    } else {
        r.handleNotFound(ctx)
    }
}
```

The results? Routes that would normally take microseconds to match now take nanoseconds. That's literally 1000x faster in some cases!

## Run Your Own Benchmarks

Want to see how amazing this performs on your own hardware? I've included comprehensive benchmarking tools:

```bash
# Run all the benchmarks
go test -bench=. -benchmem

# Just test the Get performance
go test -bench=BenchmarkLRUCache/Get -benchmem

# Focus on a specific cache size
go test -bench=Size128 -benchmem

# See how it performs in real-world scenarios
go test -bench=RealWorldWorkload -benchmem
```

## Implementation Highlights

The whole thing is just about 300 lines of highly optimized Go code. Some of my favorite tricks:

- Fixed-size array for cache entries - no slice resizing ever!
- Pre-allocated map with 2x capacity to reduce hash collisions
- Circular doubly-linked list for crazy efficient LRU operations
- Atomic counters for stats that don't need locks
- Panic recovery in critical methods because production code should never crash

## Want to Contribute?

I'd love your help making liteLRU even better! Here are some ways you can contribute:

- Find a bug? Open an issue!
- Have an idea for an optimization? Let's hear it!
- Want to improve the docs or add examples? Amazing!
- Got a cool use case? I'd love to see it!

Just follow standard Go formatting and testing practices, and we're golden.

## License

MIT License - see LICENSE file for details. Use it anywhere you want!

---

## Let's Supercharge Your Go Apps!

If you're tired of slow caches that eat memory for breakfast, give liteLRU a try. Your users (and your ops team) will thank you! Drop a star on GitHub if you find it useful, and feel free to reach out with any questions.

Happy caching! 🚀
