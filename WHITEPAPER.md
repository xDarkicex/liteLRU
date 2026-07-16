<div align="center">

# liteLRU: A Cache-Coherent, Lock-Free Approximate LRU via Chunked Bitmask Eviction on Modern Multi-Core Hardware

**Author:** xDarkicex  
**Affiliations:** libravdb · bitdev · zephyr-systems  
**Contact:** git@libravdb.com  
**Repository:** https://github.com/xDarkicex/liteLRU

---
</div>

## Abstract

We describe the design and implementation of `liteLRU`, a fixed-capacity approximate LRU cache targeting multi-core concurrent workloads. The fundamental design question is not *which items to evict*, but *how to perform eviction and recency tracking without serializing the CPU cores that share the cache*. We analyze why conventional data structures — doubly-linked lists, per-node reference counting, and AoS memory layouts — are structurally incompatible with the MESI cache coherency protocol under concurrent access. We justify the selection of off-heap memory via mmap-backed allocators to eliminate garbage collection interference with tail latency. We derive a Structure of Arrays (SoA) layout from first-principles cache-line occupancy arguments. We then derive the Chunked Bitmask CLOCK eviction algorithm from the observation that tracking recency state across 64 slots fits exactly into a 64-bit integer, enabling $O(1)$ bulk state evaluation using a single hardware `CTZ` instruction. Empirical evaluation on an 8-core ARM architecture yields p99.9 latency of 1.4 µs under a contended parallel workload.

---

## 1. Background: The Hardware Model

To reason about concurrent cache design, we first establish a precise model of the hardware execution environment.

### 1.1 The CPU Cache Hierarchy

A modern multi-core processor maintains a private L1 cache (typically 32–64 KiB, 4–5 cycle latency), a private L2 cache (256 KiB – 1 MiB, ~12 cycle latency), and a shared L3 cache (several MiB, ~40 cycle latency). Main memory sits at approximately 200–300 cycles. The performance implication is stark: a single L1 cache miss costs roughly 40× more than an L1 hit.

The fundamental unit of coherency is the **cache line**, universally 64 bytes on x86_64 and ARM64. The CPU never fetches individual bytes; it fetches and writes back entire 64-byte lines.

### 1.2 The MESI Coherency Protocol

The MESI protocol (Modified, Exclusive, Shared, Invalid) maintains coherency across cores. The relevant transitions are:

- A line held in **Shared** state by multiple cores transitions to **Invalid** in all other cores when any single core writes to it.
- A write to an **Invalid** line requires an RFO (Read For Ownership) — a broadcast on the CPU interconnect requesting exclusive ownership from all other cores.

The critical consequence is **false sharing**: if two independent variables, accessed concurrently by two different cores, reside on the same 64-byte cache line, a write by one core forces an RFO on the other core's copy, even though the second core is accessing a logically unrelated variable. This is a pure hardware serialization artifact with no algorithmic solution other than spatial separation.

### 1.3 Atomics and Memory Ordering Cost

Atomic operations (`CAS`, `fetch-and-add`) on a memory location whose cache line is not in the M (Modified) state require:
1. An MESI state transition to Exclusive via RFO.
2. The atomic read-modify-write in the modified cache line.

Under high concurrency, a single globally contested atomic (e.g., a global clock-hand pointer) causes every participating core to serialize on RFO round-trips across the CPU interconnect. As core count $N$ grows, wait time for the contested line scales as $O(N)$ per operation.

---

## 2. Why Off-Heap Memory

Go's garbage collector (GC) is a concurrent tri-color mark-and-sweep. It introduces two categories of tail-latency interference relevant to a cache implementation:

**Write Barrier Overhead.** The GC requires write barriers on every pointer store to maintain the tri-color invariant. For a structure that stores handler function pointers or string headers, every `Add` operation incurs GC bookkeeping cost proportional to the number of pointer-typed fields written.

**Stop-the-World Pauses.** While modern Go GC pause times are low (sub-millisecond), they are non-zero and non-deterministic. For a component targeting p99.9 tail latency in the low-microsecond range, any unpredictable pause is unacceptable.

**Scanner Heap Pressure.** Objects that escape to the heap are scanned during the mark phase. A cache holding thousands of string values generates a large root set for the GC to scan, increasing mark phase duration proportionally.

`liteLRU` addresses this through two mechanisms:

1. **`github.com/xDarkicex/memory.HashMap`**: An open-addressed hash map backed by an mmap-allocated region. Because mmap allocations are off the Go heap, the GC scanner does not traverse them, eliminating pointer scanning overhead for the index structure.
2. **Fixed-Width Value Arrays**: The SoA slices holding methods, paths, and params are allocated once at initialization. Strings stored within these arrays are fixed-assignment slots, not append-growing structures, bounding GC root growth to a constant.

---

## 3. Why Structure of Arrays over Array of Structs

The conventional cache entry is represented as an Array of Structs (AoS):

```
// AoS: one struct per entry
type Entry struct {
    method  string     // 16 bytes (ptr + len)
    path    string     // 16 bytes
    handler uintptr    // 8 bytes
    params  []Param    // 24 bytes
    // ... additional metadata
}
// In memory: [entry0 | entry1 | entry2 | ...]
```

Consider a sequential `Get` operation that validates 8 candidate entries by comparing their `method` and `path` fields. In AoS, each entry is `~64+ bytes`. Accessing the `method` field of entry $i$ and entry $i+1$ requires touching two separate 64-byte cache lines. For $n$ entries, this is $n$ cache-line fetches.

The Structure of Arrays (SoA) alternative separates fields into parallel arrays:

```
// SoA: one array per field
methods  []string    // [m0, m1, m2, ...] — contiguous
paths    []string    // [p0, p1, p2, ...] — contiguous
handlers []uintptr   // [h0, h1, h2, ...] — contiguous
```

When a `Get` operation scans `methods[i]` and `methods[i+1]`, both values reside in the same 64-byte cache line (a `string` header is 16 bytes, so 4 string headers per cache line). Accessing 4 consecutive methods costs 1 cache-line fetch instead of 4.

Formally, let $W_f$ be the width of field $f$ and $L = 64$ bytes be the cache line size. For AoS, the number of cache lines fetched to access field $f$ of $n$ entries is:

$$\text{Lines}_{AoS}(n, f) = n \cdot \left\lceil \frac{\text{sizeof}(\text{Entry})}{L} \right\rceil$$

For SoA:

$$\text{Lines}_{SoA}(n, f) = \left\lceil \frac{n \cdot W_f}{L} \right\rceil$$

Since $W_f \ll \text{sizeof}(\text{Entry})$, we have $\text{Lines}_{SoA}(n, f) \ll \text{Lines}_{AoS}(n, f)$ for all $n > 1$.

---

## 4. Why Bitsets for Recency Tracking

### 4.1 The Cost of Pointer-Based Recency

Traditional LRU tracks recency by maintaining a doubly-linked list ordered from Most Recently Used (MRU) to Least Recently Used (LRU). A cache hit requires:
1. Unlinking the accessed node from its current list position: 4 pointer writes.
2. Relinking it at the MRU head: 4 pointer writes.

Each pointer write on a concurrent system must be protected by a mutex. On a 2-core system, this serializes all `Get` operations behind a single lock. As core count grows, lock contention grows monotonically.

Lock-free linked-list variants exist but require hazard pointers or epoch-based reclamation to prevent use-after-free races. These mechanisms add per-operation overhead and complexity that reintroduces serialization on shared epoch counters.

### 4.2 CLOCK as an Approximation

The CLOCK algorithm approximates LRU by abandoning exact recency ordering. Instead of tracking *which* entry is least recently used, it tracks *whether* each entry has been accessed since the last eviction sweep. This is a single bit per entry.

Upon `Get`, an entry's reference bit is set to 1.
Upon eviction, a hand sweeps the array. If the reference bit is 1, it is cleared to 0 (the entry gets a "second chance"). If the reference bit is 0, the entry is selected as the eviction victim.

This approximates LRU behavior without pointer manipulation. The reference bit can be set with a non-blocking atomic OR, and cleared with an atomic AND. No serializing mutex is required.

### 4.3 From One Bit to One 64-bit Integer

Standard CLOCK still maintains a global clock-hand pointer — an atomic integer indicating the current sweep position. Every eviction increments this counter and examines a single entry. Under high concurrency, all evicting threads contend on this single atomic, reintroducing the same RFO serialization we wished to avoid.

The key observation driving `liteLRU`'s design: if we partition the cache into chunks of exactly 64 entries, the valid state and access state of all 64 entries in a chunk can be represented as two 64-bit integers:

- $V_k \in \{0,1\}^{64}$: bit $i = 1$ iff slot $i$ holds a valid entry.
- $A_k \in \{0,1\}^{64}$: bit $i = 1$ iff slot $i$ has been accessed since last sweep.

A slot is an eviction candidate if it is empty ($V_k[i] = 0$) or valid but unaccessed ($V_k[i] = 1 \land A_k[i] = 0$). The set of all candidates across all 64 slots is computed in a single bitwise expression:

$$C_k = \neg V_k \;\lor\; (V_k \;\land\; \neg A_k)$$

The index of the first candidate is then extracted using the hardware `CTZ` (Count Trailing Zeros) instruction, a single-cycle hardware primitive on both x86_64 (`TZCNT`) and ARM64 (`RBIT` + `CLZ`):

$$i = \text{CTZ}(C_k)$$

This replaces an $O(N)$ sequential scan over $N$ atomic loads with two 64-bit register operations and one hardware intrinsic, bounding eviction search to a strict hardware-constant time within any chunk.

### 4.4 Eliminating the Global Clock Hand

Because `liteLRU` seeds the starting chunk index from the hash of the incoming route, $k_{start} = h(r) \bmod N_k$, there is no global clock-hand atomic. Each thread begins its eviction sweep at a deterministic but distributed starting position. Contention on a shared sweep counter is eliminated entirely.

---

## 5. Sequence Lock Design for Wait-Free Reads

We require that `Get` operations never block on an ongoing `Add`, but also never return torn data if an eviction overwrites a slot mid-read.

A **Sequence Lock** (Seqlock) achieves this. Each slot $i$ maintains an atomic 32-bit sequence counter $S_i$, initialized to 0.

**Write Protocol:** Before mutating slot $i$, a writer sets $S_i \leftarrow S_i + 1$ (making it odd). After completing the write, it sets $S_i \leftarrow S_i + 1$ (returning to even).

**Read Protocol:** A reader samples $s_1 \leftarrow S_i$. If $s_1$ is odd, the slot is being written; report a miss. Otherwise, read the data. Sample $s_2 \leftarrow S_i$. If $s_1 \neq s_2$, the slot was concurrently modified; report a miss. Otherwise, the read is consistent.

**Theorem (Wait-Free Reads).** The `Get` operation is wait-free: it always completes in a bounded number of steps, regardless of concurrent writer behavior.

*Proof.* The reader never waits on a lock or loops. At any point where a concurrent write would invalidate the read, the reader detects the condition and immediately returns a miss. The total number of steps is bounded by a constant (two atomic loads, two string comparisons, one bitmask check). $\square$

---

## 6. Cache-Line Padding to Eliminate False Sharing

A naive implementation allocates seqlocks as a contiguous slice `[]atomic.Uint32`. Each `uint32` is 4 bytes, placing 16 seqlocks per 64-byte cache line. A concurrent write to slot $j$ invalidates the cache line also holding $S_{j+1}, \ldots, S_{j+15}$ across all other cores, even if those slots are being independently read.

**Padding Theorem.** If each $S_i$ occupies exactly one 64-byte cache line, concurrent operations on any two distinct slots $i \neq j$ produce no MESI coherency traffic between the cache lines of $S_i$ and $S_j$.

*Proof.* Let $\text{addr}(S_i)$ denote the base address of the padded slot state for slot $i$. We define:

```go
type slotState struct {
    seq atomic.Uint32 //  4 bytes
    _   [60]byte      // 60 bytes padding
}                     // sizeof = 64 bytes exactly
```

Since $\text{addr}(S_{i+1}) = \text{addr}(S_i) + 64$, adjacent entries reside on disjoint, non-overlapping 64-byte aligned cache lines. A write to $S_i$ sets the MESI state of line $\lfloor \text{addr}(S_i) / 64 \rfloor$ to Modified, which does not affect the state of line $\lfloor \text{addr}(S_j) / 64 \rfloor$ for any $j \neq i$. False sharing is structurally impossible. $\square$

The same argument applies to the statistics counters. Aggregating hits and misses into two global `atomic.Int64` values would cause every `Get` on every core to contend on two shared cache lines. We instead shard into 64 independent `statStripe` structs, each padded to 64 bytes, selected by `hash & 63`. Each core writes exclusively to its own stripe with zero coherency interference.

---

## 7. The Measurement Artifact of `b.RunParallel`

Standard Go micro-benchmarks use `b.RunParallel`, which distributes iterations of the benchmark loop across goroutines via an internal `pb.Next()` call. `pb.Next()` performs an atomic fetch-and-add on a shared 64-bit counter to assign work to each goroutine.

When the operation under test completes in sub-10ns — as is typical for a cache hit — the benchmark overhead from the `pb.Next()` atomic dominates. According to Amdahl's Law, the maximum parallel speedup for a workload with sequential fraction $f$ across $N$ cores is:

$$S(N) = \frac{1}{(1-f) + \dfrac{f}{N}}$$

When the operation time $T_{op} \approx T_{atomic}$, the sequential fraction approaches $f \to 1$. In this limit, $S(N) \to 1$ regardless of $N$, producing the appearance of flat or declining throughput. This is a benchmarking artifact, not a reflection of the algorithm's scaling behavior.

Our custom latency test bypasses `pb.Next()` entirely. Each goroutine operates a tight independent loop over a pre-allocated, fixed-size path array with no shared iteration counter. This removes the artifact and measures only the cache mechanics.

---

## 8. Evaluation

**Platform:** Apple M2, 8-core ARM64, macOS. Go 1.25.7. Cache capacity 1024 entries, maxParams 20.

**Workload:** 8 concurrent workers, $1.6 \times 10^6$ total operations, 80% `Get` / 20% `Add`, paths drawn from a pool of 1000 distinct routes producing ~70% hit ratio.

### 8.1 Throughput Scaling

Using the un-synchronized worker methodology:

| Cores | Ops/sec       | Scaling Factor |
|------:|--------------|----------------|
| 1     | 5,457,508    | 1.00×          |
| 2     | 8,847,292    | 1.62×          |
| 4     | 13,701,548   | 2.51×          |
| 8     | 17,066,659   | 3.13×          |

The sub-linear scaling from 4 to 8 cores is expected: at 8 cores on an M2 die, the shared L3 and memory bus bandwidth begin to constrain throughput independently of lock contention.

### 8.2 Latency Percentiles

Measured with an inherent ~40ns `time.Now()` overhead per sample:

| Percentile | Measured  | Estimated (overhead removed) |
|:----------:|----------:|-----------------------------:|
| p50        | 250 ns    | ~210 ns                      |
| p99        | 1,000 ns  | ~960 ns                      |
| p99.9      | 1,417 ns  | ~1,377 ns                    |
| Max        | ~7.1 ms   | —                            |

The max is dominated by OS scheduling jitter on at least one of the 8 goroutines, not cache mechanics.

---

## 9. Discussion and Limitations

**Approximate Eviction.** The Chunked Bitmask CLOCK algorithm is a CLOCK approximation of LRU, not true LRU. In workloads with adversarial access patterns specifically targeting the CLOCK approximation error, hit-rate may degrade relative to a true LRU implementation. The tradeoff is accepted: structural lock freedom is valued over exact eviction fidelity.

**Fixed Capacity.** The cache does not resize. The hash map is pre-allocated. This is a deliberate design decision to prevent any allocation or growth operation during steady-state execution, but it requires the caller to provision capacity at initialization.

**String Copying.** While `Get` is zero-allocation for the routing lookup itself, returning a `[]Param` copy requires a pool-managed allocation. The pool bounds this cost, but it is not zero in all cases.

---

## 10. Conclusion

We have presented the hardware-grounded reasoning behind each principal design decision in `liteLRU`: off-heap allocation to eliminate GC interference, SoA layout to maximize cache-line occupancy, bitset-encoded state to enable bulk $O(1)$ evaluation via hardware intrinsics, seqlock sequencing to provide wait-free read semantics, and cache-line padding to eliminate false-sharing serialization. The combination yields a cache architecture whose concurrency cost is bounded by single-cycle hardware instructions rather than operating system synchronization primitives.
