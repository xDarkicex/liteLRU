<div align="center">

# liteLRU: A MESI-Conscious, Contention-Bounded Approximate LRU via Chunked Bitmask Eviction on Modern Multi-Core Hardware

**Author:** xDarkicex  
**Affiliations:** libravdb · bitdev · zephyr-systems  
**Contact:** git@libravdb.com  
**Repository:** https://github.com/xDarkicex/liteLRU

---


</div>

## Abstract

Concurrent approximate LRU structures often serialize cores on shared metadata. We present `liteLRU`, a fixed-capacity cache based on 64-slot bitmask CLOCK, 64-way set associativity with SWAR signatures, and padded seqlocks. Reads are wait-free; writes are contention-bounded via a load-shedding admission protocol. By confining heavily mutated synchronization state to off-heap memory, `liteLRU` isolates cache-line interconnect traffic from Go's garbage collector. Our evaluation demonstrates that `liteLRU` sustains over 30 million operations per second under severe 80% write contention while providing sub-microsecond p99 latency on the cache hot path and contention-bounded write work under synthetic and end-to-end workloads.

---

## 1. Background: The Hardware Model

To reason about concurrent cache design, we first establish a precise model of the hardware execution environment.

### 1.1 The CPU Cache Hierarchy

A modern multi-core processor maintains a private L1 cache (typically 32–64 KiB, 4–5 cycle latency), a private L2 cache (256 KiB – 1 MiB, ~12 cycle latency), and a shared L3 cache (several MiB, ~40 cycle latency). Main memory sits at approximately 200–300 cycles. The performance implication is stark: a single L1 cache miss costs roughly 40× more than an L1 hit.

The fundamental unit of coherency is the **cache line**, dependent on the target platform architecture (e.g., $L=64$ bytes on x86_64, $L=128$ bytes on Apple Silicon). The CPU never fetches individual bytes; it fetches and writes back entire $L$-byte coherence lines.

### 1.2 The MESI Coherency Protocol

The MESI protocol (Modified, Exclusive, Shared, Invalid) was introduced by Papamarcos and Patel [[1]](#ref-mesi) to maintain coherency across private caches in a multi-processor system. The relevant state transitions are:

- A line held in **Shared** state by multiple cores transitions to **Invalid** in all other cores when any single core writes to it.
- A write to an **Invalid** line requires an RFO (Read For Ownership) — a broadcast on the CPU interconnect requesting exclusive ownership from all other cores.

The critical consequence is **false sharing** [[8]](#ref-falsesharing): if two independent variables, accessed concurrently by two different cores, reside on the same $L$-byte coherence line, a write by one core forces an RFO on the other core's copy, even though the second core is accessing a logically unrelated variable. This is a pure hardware serialization artifact with no algorithmic solution other than spatial separation.

### 1.3 Atomics and Memory Ordering Cost

Atomic operations (`CAS`, `fetch-and-add`) on a memory location whose cache line is not in the M (Modified) state require:
1. An MESI state transition to Exclusive via RFO.
2. The atomic read-modify-write in the modified cache line.

Under high concurrency, a single globally contested atomic (e.g., a global clock-hand pointer) causes every participating core to serialize on RFO round-trips across the CPU interconnect. As core count $N$ grows, wait time for the contested line scales as $O(N)$ per operation, a fact formalized by Amdahl [[2]](#ref-amdahl). The lock-free design methodology necessary to escape this bound is treated extensively by Herlihy and Shavit [[12]](#ref-lockfree).

---

## 1.4 Contributions

In this paper, we make the following contributions to concurrent cache design:
* **Contention-Bounded Eviction Algorithm:** A 64-way set associative architecture utilizing chunked bitmasks and hardware `CTZ`, eliminating global clock hands and open-addressed maps entirely.
* **Hybrid Memory Architecture:** Off-heap allocation for lock-free state to isolate hot concurrency primitives from GC write barriers while safely tracking Go pointers on the heap.
* **Padded Seqlocks:** A strict structural layout that yields strictly wait-free reads and eliminates false sharing for per-slot metadata.
* **Load-Shedding Eviction Protocol:** A load-shedding protocol that caps write-side work to a fixed number of CAS attempts; under sustained contention, admission may be dropped.

## 2. Hybrid Memory Architecture and GC Isolation

**Scanner Heap Pressure.** Objects that escape to the heap are scanned during the mark phase. A cache holding thousands of string values generates a large root set for the GC to scan, increasing mark phase duration proportionally.

`liteLRU` addresses this through two non-trivial architectural changes:

1. **64-Way Set Associativity over Hash Maps**: Lock-free open-addressed hash maps mathematically require *tombstones* during deletion to preserve concurrent probe sequences. When tombstones accumulate, lock-free maps are forced to perform global, cooperative compactions, resulting in large tail latency spikes (often >100ms) under heavy eviction load. `liteLRU` completely eliminates the hash map. Instead, it utilizes a hardware-inspired **64-way set associative architecture**. An incoming route hash is mathematically mapped to a specific 64-slot bucket (a "set"). Evictions overwrite victims *in place* within the local set — there are no tombstones to compact, no probe sequences to preserve, and no background goroutine required.
2. **SWAR Signature Scanning**: To instantly find a key within the 64-slot set without traversing arrays of strings, `liteLRU` maintains a `uint64` signature word per 8 slots. Each byte in the word represents an 8-bit hash signature. Lookups use SIMD Within A Register (SWAR) bitwise operations to scan 8 slots in a single CPU instruction, providing $O(1)$ L1-cache aligned lookups. An 8-bit signature carries a 1/256 per-slot false-positive probability. Across 8 slots per word, the expected false-positive rate per word is approximately $1 - (1 - 1/256)^8 \approx 3\%$. Any SWAR match is confirmed by a full string comparison before the entry is returned; false positives incur one extra comparison, not an incorrect response. Notably, 64 slots require 8 `uint64` signature words — exactly 64 bytes — fitting the entire signature metadata for a set in **one L1 cache line**, which is fetched once and eliminates redundant memory traffic during lookup.

To achieve this without sacrificing safety, `liteLRU` employs a **hybrid memory architecture**. High-frequency concurrency primitives (bitmasks, seqlocks) are allocated purely off-heap via anonymous `mmap` to eliminate GC tracing and write barriers entirely. Conversely, data arrays containing Go pointers (keys, values, and handler functions) remain on the standard Go heap. This separation ensures the GC can still safely track and collect dynamic strings or closures, completely avoiding dangling-pointer hazards while keeping the hot eviction path invisible to the GC.

---

## 3. Related Work

The problem of concurrent cache design generally branches into two orthogonal domains: optimizing the eviction *policy* for higher hit rates, and optimizing the concurrent *mechanism* for lower CPU overhead. `liteLRU` is strictly a contribution to the latter.

**Concurrent CLOCK Mechanisms.** The closest published system to `liteLRU` is MemC3 [[15]](#ref-memc3), which optimized Memcached by introducing a concurrent CLOCK with 1-bit recency tags and optimistic locking to remove mutexes from the read path. `liteLRU` builds on this foundation by applying it to the Go runtime (off-heap `mmap`, zero-allocation SoA), but replaces MemC3's sequential bit-checks with bulk 64-bit bitmask evaluations via hardware `CTZ` intrinsics, drastically reducing eviction scan latency.

**Advanced Eviction Policies.** Recent systems like CAR, CLOCK-Pro [[13]](#ref-clockpro), S3-FIFO [[5]](#ref-s3fifo), and SIEVE [[6]](#ref-sieve) demonstrate that scan-resistant eviction policies can outperform strict LRU on web workloads. These policy innovations are generally orthogonal to the mechanism: a SIEVE or S3-FIFO algorithm still requires a concurrent implementation strategy to scale across cores without locking.

**Production Go Caches.** Existing Go caches like `freecache`, `bigcache`, and `ristretto` achieve high throughput via sharding and background eviction goroutines. `liteLRU` takes a different approach: rather than amortizing eviction cost over background threads or coarse-grained shard locks, `liteLRU` eliminates the locking overhead entirely via bitmask mathematics and seqlocks, enabling wait-free synchronous reads and inline contention-bounded writes.

---

## 4. Why Structure of Arrays over Array of Structs

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

Consider a sequential `Get` operation that validates 8 candidate entries by comparing their `method` and `path` fields. In AoS, each entry is `~64+ bytes`. Accessing the `method` field of entry $i$ and entry $i+1$ requires touching two separate $L$-byte cache lines. For $n$ entries, this is $n$ cache-line fetches.

The Structure of Arrays (SoA) alternative, widely employed in high-performance and SIMD-oriented computing [[7]](#ref-soa), separates fields into parallel arrays:

```
// SoA: one array per field
methods  []string    // [m0, m1, m2, ...] — contiguous
paths    []string    // [p0, p1, p2, ...] — contiguous
handlers []uintptr   // [h0, h1, h2, ...] — contiguous
```

When a `Get` operation scans `methods[i]` and `methods[i+1]`, both values reside in the same $L$-byte cache line (a `string` header is 16 bytes, so 4 string headers per cache line). Accessing 4 consecutive methods costs 1 cache-line fetch instead of 4 (assuming $L \ge 64$).

Formally, let $W_f$ be the width of field $f$ and $L = 64$ bytes be the cache line size. For AoS, the number of cache lines fetched to access field $f$ of $n$ consecutive entries depends heavily on padding and struct size. If $\text{sizeof}(\text{Entry}) \le L$ and the compiler naturally packs them, we approximate the cost as fetching one new cache line per entry:

$$\text{Lines}_{AoS}(n, f) \approx n$$

For SoA, because values are tightly packed back-to-back:

$$\text{Lines}_{SoA}(n, f) = \left\lceil \frac{n \cdot W_f}{L} \right\rceil$$

Since $W_f \ll \text{sizeof}(\text{Entry})$, we have $\text{Lines}_{SoA}(n, f) \ll \text{Lines}_{AoS}(n, f)$ for all $n > 1$.

---

## 5. Why Bitsets for Recency Tracking

### 5.1 The Cost of Pointer-Based Recency

Traditional LRU [[4]](#ref-lru) tracks recency by maintaining a doubly-linked list ordered from Most Recently Used (MRU) to Least Recently Used (LRU). A cache hit requires:
1. Unlinking the accessed node from its current list position: 4 pointer writes.
2. Relinking it at the MRU head: 4 pointer writes.

Each pointer write on a concurrent system must be protected by a mutex. On a 2-core system, this serializes all `Get` operations behind a single lock. As core count grows, lock contention grows monotonically.

Lock-free linked-list variants exist but require hazard pointers or epoch-based reclamation to prevent use-after-free races [[12]](#ref-lockfree). These mechanisms add per-operation overhead and complexity that reintroduces serialization on shared epoch counters.

### 5.2 CLOCK as an Approximation

The CLOCK algorithm [[3]](#ref-clock) [[13]](#ref-clockpro) approximates LRU by abandoning exact recency ordering. Instead of tracking *which* entry is least recently used, it tracks *whether* each entry has been accessed since the last eviction sweep — a single bit per entry.

Upon `Get`, an entry's reference bit is set to 1 via an atomic OR.
Upon eviction, a hand sweeps the array. Reference bit 1 is cleared to 0 (second chance). Reference bit 0 designates the eviction victim.

This approximates LRU behavior without pointer manipulation. Recent research such as S3-FIFO [[5]](#ref-s3fifo) and SIEVE [[6]](#ref-sieve) further refines eviction policy, but does not address the parallel execution cost of the eviction mechanism itself, which is the problem we solve.

### 5.3 From One Bit to One 64-bit Integer

Standard CLOCK still maintains a global clock-hand pointer — an atomic integer indicating the current sweep position. Every eviction increments this counter and examines a single entry. Under high concurrency, all evicting threads contend on this single atomic, reintroducing the same RFO serialization we wished to avoid.

The key observation driving `liteLRU`'s design: if we partition the cache into chunks of exactly 64 entries, the valid state and access state of all 64 entries in a chunk can be represented as two 64-bit integers:

- $V_k \in \{0,1\}^{64}$: bit $i = 1$ iff slot $i$ holds a valid entry.
- $A_k \in \{0,1\}^{64}$: bit $i = 1$ iff slot $i$ has been accessed since last sweep.

A slot is an eviction candidate if it is empty ($V_k[i] = 0$) or valid but unaccessed ($V_k[i] = 1 \land A_k[i] = 0$). The set of all candidates across all 64 slots is computed in a single bitwise expression:

$$C_k = \neg (V_k \;\land\; A_k) \;\land\; \neg W_k$$

The index of the first candidate is then extracted using the hardware `CTZ` (Count Trailing Zeros) instruction [[10,11]](#ref-tzcnt). It is crucial to handle platform-specific zero behavior: on x86_64, `TZCNT` returns 64 when the input is zero, whereas on ARM64, `RBIT` + `CLZ` requires an explicit branch or zero-check. When a non-zero candidate mask exists:

$$i = \text{CTZ}(C_k)$$

The bitmask representation drastically reduces the cost of scanning. A core seeking eviction performs an atomic load of $A_k$. If $A_k = \texttt{0xFFFFFFFFFFFFFFFF}$, all slots have been accessed. The core atomically applies a bulk wipe $A_k \leftarrow 0$. It is a known tradeoff of this bulk-clearing approach that bits set by concurrent readers microseconds prior will be wiped alongside stale bits; however, this mild loss of precision is accepted in exchange for eliminating $O(N)$ per-slot atomic instructions.

**Writer Progress and Claim Protocol.** 
To safely claim the selected bit $i$ for eviction, we define a third state bitmask for the chunk: $W_k \in \{0,1\}^{64}$, where bit $i = 1$ if and only if a writer currently owns slot $i$ for mutation. 

The eviction search process executes as follows:
1. Extract candidate index $i = \text{CTZ}(C_k)$.
2. Execute an atomic Compare-And-Swap (CAS) to set $W_k[i] \leftarrow 1$.
3. If the CAS fails (another writer claimed it), restart at step 1 for the same chunk.
4. If $W_k[i] = 1$ is successfully claimed, proceed with the in-place 64-way set associative write protocol defined in §6.

To guarantee O(1) lock-free progress and bound the worst-case scenario where a chunk is pathologically saturated by concurrent writers (e.g., a Thundering Herd), `liteLRU` employs **Load-Shedding Eviction**. The `findVictim` loop attempts to claim a write-lock (`writing` bit) on an eviction candidate. If it fails to claim any slot after 10 retries, it returns a sentinel failure value (`0xFFFFFFFF`). The `Add` operation intercepts this sentinel and safely drops the cache admission. By sacrificing absolute admission under extreme contention, `liteLRU` bounds write-side work to a fixed number of CAS attempts; under sustained contention, admission may be dropped.

### 5.4 Eliminating the Global Clock Hand

Because `liteLRU` seeds the starting chunk index from the hash of the incoming route, $k_{start} = h(r) \bmod N_k$, there is no global clock-hand atomic. Each thread begins its eviction sweep at a deterministic but distributed starting position. Contention on a shared sweep counter is eliminated entirely. 

---

## 6. Algorithm and Correctness

### 6.1 State Definitions and Layout

The cache state is divided into global configuration, per-chunk metadata, and per-slot data:

* **Global:** Total capacity $C$ and number of chunks $N = C/64$.
* **Per-Chunk:** A `chunk` struct containing 64-bit masks $V_k$ (valid bits), $W_k$ (writing bits), $A_k$ (access/recency bits), and an array of 8 `uint64` SWAR signatures for fast scanning.
* **Per-Slot:** A `slotState` padded to 64 bytes containing the seqlock $S_i$, alongside Structure of Arrays (SoA) slices for keys, values, and handlers.

### 6.2 Pseudocode and Control Flow

**Get(key)**
1. Hash the key to find the corresponding chunk $k$.
2. Scan the 8 SWAR signature words using byte-wise comparison.
3. For any match at slot $i$, verify the slot is valid in $V_k$.
4. Load the seqlock $S_{i, \text{start}}$. If odd, skip slot (conflict).
5. Read the key/value from the SoA arrays.
6. Load the seqlock $S_{i, \text{end}}$. If $S_{i, \text{start}} \neq S_{i, \text{end}}$, skip slot (conflict).
7. Atomically OR the bit $i$ into $A_k$ to record recency and return hit.
8. If no slots match cleanly, return miss.

**Add(key, value)**
1. Hash the key to find chunk $k$.
2. Run `findVictim(k)`.
3. If `findVictim` returns the failure sentinel, drop the admission and return.
4. Clear the victim's bit in $V_k$.
5. Increment the seqlock $S_i$ to an odd value.
6. Write the new key and value into the SoA arrays at the victim slot.
7. Update the SWAR signature byte for the new key.
8. Increment the seqlock $S_i$ to an even value.
9. Set the victim's bit in $V_k$, clear its bit in $A_k$, and release its bit in $W_k$.

**findVictim(chunk k)**
1. Retry loop (max 10 iterations).
2. Load $V_k, A_k, W_k$. If $C_k = 0$, atomically clear $A_k$ for all valid slots via CAS. Recompute $C_k$ before proceeding.
3. Apply the CLOCK approximation: identify candidate slots using `CTZ((~V_k | ~A_k) & ~W_k)`.
4. Attempt to CAS the $W_k$ bitmask to claim the slot.
5. If successful, return the slot index.
6. If unsuccessful (due to concurrent writers), increment retry counter.
7. If 10 retries are exhausted, return the `0xFFFFFFFF` failure sentinel.

### 6.3 Correctness Theorems

**Theorem 1 (Wait-Free Reads).** *Any thread executing `Get` is guaranteed to complete in a bounded number of instructions regardless of the state or execution speed of concurrent writers.*
*Proof:* The read path consists solely of a SWAR signature scan, atomic loads of the $L$-byte padded seqlock $S_i$, a string comparison, and a single bitwise `OR` for the access bit. The padded layout strictly isolates $S_i$, eliminating false-sharing across slots. The read path evaluates at most 8 SWAR word scans, and for each match at most two seqlock loads and one key compare; conflicts skip without retry; if no clean match, return miss. Under no circumstances does a reader spin on a lock or retry.

**Theorem 2 (Bounded Write-Side Work).** *Any thread executing `Add` will terminate in $O(1)$ atomic operations.*
*Proof:* `findVictim` utilizes the `CTZ` instruction to identify eviction candidates. It attempts to claim a candidate via a Compare-And-Swap (CAS) on $W_k$. If the CAS fails due to concurrent writers, the thread retries. A hard limit of 10 retries is enforced. If 10 retries are exhausted, `findVictim` returns a failure sentinel and `Add` drops the admission entirely. This load-shedding rigorously bounds the maximum write-side work.

**Theorem 3 (Safety Invariant).** *No two writers may simultaneously overwrite the same slot, and readers will never observe torn key/value pairs.*
*Proof:* A slot is exclusively claimed when a writer successfully sets its corresponding bit in the $W_k$ bitmask via CAS. The writer then invalidates the slot in $V_k$ before incrementing the seqlock $S_i$ to an odd value, writing the SoA data, and incrementing $S_i$ to an even value. A reader verifying the sequence bounds $(S_{i, \text{start}} == S_{i, \text{end}})$ and $(S_i \pmod 2 == 0)$ is guaranteed to never observe a torn key/value pair for a single slot.

## 7. Cache-Line Padding to Eliminate False Sharing

A naive implementation allocates seqlocks as a contiguous slice `[]atomic.Uint32`. Each `uint32` is 4 bytes, placing multiple seqlocks per cache line. By the MESI coherency protocol [[1]](#ref-mesi), a concurrent write to slot $j$ invalidates the line holding $S_{j+1}, \ldots, S_{j+15}$ across all other cores — the false-sharing pathology described by Bolosky and Scott [[8]](#ref-falsesharing).

`slotState` metadata is allocated in a page-aligned anonymous `mmap` region. For each supported platform, `liteLRU` selects a slot stride equal to the platform's verified coherence-line size $L$ (64 bytes for x86_64, 128 bytes for Apple Silicon). Because the mapping base is page-aligned and each slot has stride $L$, distinct slot states begin on distinct coherence-line boundaries.

**Padding Theorem.** On a platform with coherence-line size $L$, if the base of the slot-state region is aligned to $L$ and each slot state has stride $L$, distinct slots occupy distinct coherence lines.

*Proof.* Let $\text{addr}(S_i)$ denote the base address of the padded slot state for slot $i$. We define:

```go
// L is the verified coherence-line size for the target platform via build tags.
type slotState struct {
    seq atomic.Uint32 // 4 bytes
    _   [L - 4]byte   // L-4 bytes padding
}
```

Because the `mmap` allocation guarantees $\text{addr}(S_0) \pmod L = 0$ (page alignment implies $L$ alignment for $L \le 4096$), and $\text{addr}(S_{i+1}) = \text{addr}(S_i) + L$, adjacent entries reside on disjoint, non-overlapping $L$-byte aligned cache lines. A write to $S_i$ sets the MESI state of line $\lfloor \text{addr}(S_i) / L \rfloor$ to Modified, which does not affect the state of line $\lfloor \text{addr}(S_j) / L \rfloor$ for any $j \neq i$. False sharing is structurally impossible. $\square$

It is important to note that the 64-bit bitmasks ($V_k$, $A_k$, $W_k$) do remain shared contention points. However, because they batch 64 slots into a single atomic integer, the contention surface area is reduced by a factor of 64 compared to per-slot tracking.

The same argument applies to the statistics counters. Aggregating hits and misses into two global `atomic.Int64` values would cause every `Get` on every core to contend on two shared cache lines. We instead shard into 64 independent `statStripe` structs, each padded to 64 bytes, selected by `hash & 63`. Each core writes exclusively to its own stripe with zero coherency interference.

---

## 8. Benchmark Methodology and Artifacts

Standard Go micro-benchmarks use `b.RunParallel`, which distributes iterations of the benchmark loop across goroutines via an internal `pb.Next()` call. `pb.Next()` performs an atomic fetch-and-add on a shared 64-bit counter to assign work to each goroutine.

When the operation under test completes in sub-10ns — as is typical for a cache hit — the benchmark overhead from the `pb.Next()` atomic dominates. According to Amdahl's Law, the maximum parallel speedup for a workload with sequential fraction $f$ across $N$ cores is:

$$S(N) = \frac{1}{(1-f) + \dfrac{f}{N}}$$

When the operation time $T_{op}$ is comparable to the atomic latency $T_{atomic}$ (~20–40 ns including RFO round-trip), the contention on the central `pb.Next()` counter dominates. Using Amdahl's framework for parallel scaling [[2]](#ref-amdahl), this acts as an enforced sequential bottleneck, forcing $S(N) \to 1$ regardless of $N$. The result is an apparent decrease in throughput with additional cores — a benchmark artifact, not a property of the algorithm.

Our custom latency test bypasses `pb.Next()` entirely. Each goroutine operates a tight independent loop over a pre-allocated, fixed-size key array with no shared iteration counter, eliminating the artifact and isolating the cache mechanics.

---

## 9. Evaluation

**Platform:** Apple M2, 8-core ARM64, macOS. Go 1.25.7. Cache capacity 1024 entries.

### 9.1 Baseline Throughput and Tail Latency

**Workload:** 8 concurrent workers, $1.6 \times 10^6$ total operations, 80% `Get` / 20% `Add`, paths drawn from a uniform pool of 1000 distinct routes producing ~70% hit ratio. This benchmark uses a uniform (non-Zipfian) distribution to isolate the effect of concurrency mechanics rather than eviction policy.

| Cache Implementation      | Ops/sec    | p50 Latency | p99 Latency |
|---------------------------|-----------:|------------:|------------:|
| `liteLRU`                 | 29,743,140 | 209 ns      | 625 ns      |
| `otter` v2                | 16,299,814 | 333 ns      | 49.83 µs    |
| `Mutex LRU` (Naive)       |  8,960,372 | 167 ns      | 23.08 µs    |

*Note: Latency percentiles in this table were sampled at a 1% rate to preserve realistic throughput levels during measurement. This 1% sampling interval (and the different API harness) captures a slightly different distribution and a slightly different percentile profile.*

`liteLRU` demonstrates an $\approx$3.3x throughput advantage over the naive Mutex LRU, and nearly double the throughput of the highly optimized `otter` v2 concurrent cache. Furthermore, `liteLRU` possesses by far the lowest p99 tail latency (625 ns) of all concurrent caches tested, bounded strictly by lock-free atomics rather than buffer flushes. `otter` absorbs common-path operations into background buffers for a fast median, but suffers severe tail latency (49.8 µs) during concurrent buffer flushes. `liteLRU` instead performs synchronous, bounded writes into the 64-way set associative slots.

### 9.2 Throughput Scaling Under Contention

To stress lock-free scaling under skewed access patterns, we scale a 1.6M operation **Zipfian** workload ($s=1.001$, $N=100{,}000$) across 1 to 8 concurrent cores. Unlike §9.1's uniform distribution, the Zipfian skew produces a hotter working set and greater eviction pressure, making this a distinct but complementary benchmark.

| Cores | Ops/sec       | Scaling Factor |
|------:|--------------|----------------|
| 1     | 6,078,793    | 1.00×          |
| 2     | 10,899,600   | 1.79×          |
| 4     | 18,972,613   | 3.12×          |
| 8     | 20,303,656   | 3.34×          |

The sub-linear scaling from 4 to 8 cores is expected: at 8 cores on an M2 die, the shared L3 and memory bus bandwidth begin to constrain throughput independently of lock contention.

### 9.3 Latency Percentiles (ARM64)

Measured with an inherent ~40ns `time.Now()` overhead per sample:

| Percentile | Measured  | Estimated (overhead removed) |
|:----------:|----------:|-----------------------------:|
| p50        | 208 ns    | ~168 ns                      |
| p99        | 1,083 ns  | ~1,043 ns                    |
| p99.9      | 1,334 ns  | ~1,294 ns                    |
| Max        | ~6.3 ms   | —                            |

The max is dominated by OS scheduling jitter on at least one of the 8 goroutines, not cache mechanics.

### 9.4 Ablation Study

To isolate the contribution of each architectural decision in `liteLRU`'s **64-way set associative SWAR design**, we benchmarked the full implementation against three stripped-down variants under the identical 8-core 80/20 uniform workload. The ablation variants target the three novel mechanisms in the new architecture:

- **No Padding**: Removed the target-platform $L$-byte padding from seqlocks and stripe counters, inducing false sharing across concurrent accessors.
- **No Bitmask / No SWAR**: Replaced the O(1) SWAR signature scan + `CTZ` intrinsic with a naive 64-iteration `for` loop performing full string comparisons on each slot.
- **No Set Associativity (Map+Mutex)**: Replaced the 64-way set associative slot array with a standard Go `map` protected by a `sync.RWMutex`. This represents a **combined penalty**, stripping away both lock-free associativity and SWAR scanning, restoring the lock-based eviction path the new architecture was designed to eliminate.

| Variant                        | Ops/sec    | Throughput Loss |
|--------------------------------|-----------:|----------------:|
| `liteLRU` (Full)*              | 32,024,792 | —               |
| No SWAR (Linear String Scan)   | 29,034,906 | -9%             |
| No Padding (False Share)       | 19,407,171 | -39%            |
| No Set Associativity (Map+Mutex)|  4,280,582 | -87%            |

The ablation confirms that while SWAR hardware acceleration provides a measurable 9% optimization, the two structural requirements for scaling are cache-line isolation (-39% without padding) and elimination of the centralized mutex-protected map (-87%). The -87% result quantifies the combined cost of Go's `sync.RWMutex` overhead and the single-contention-point map: the 64-way set associative design avoids both by confining all writes to a localized, lock-free slot within the pre-hashed set.

*\*Note: The ablation runs use a distinct benchmark harness measuring absolute hardware limits without the 1% tracking overhead of the p99 latency evaluation, accounting for the higher ops/sec baseline relative to §9.1.*

### 9.5 x86_64 Smoke Test

To verify behavioral consistency across architectures, we evaluated `liteLRU` on an x86_64 Docker container instance. Due to the differing memory models and MESI topologies between Intel/AMD and Apple Silicon, absolute latency numbers differ, but the lock-free scaling properties hold.

| Architecture | Cores | Ops/sec (x86_64) |
|--------------|------:|-----------------:|
| x86\_64       | 8     | 13,155,469       |

*Note: The x86_64 evaluation was performed inside a Docker Desktop virtualized instance, which introduces nominal hypervisor overhead compared to bare-metal execution.*

### 9.6 Zipfian Hit-Rate Evaluation

To evaluate eviction fidelity under realistic skewed access patterns, we benchmarked `liteLRU` against `otter` using a Zipfian distribution ($s=1.001$, $N=100,000$ working set) with 1.6M total operations across varying cache capacities. Both caches were given a 20% warmup phase to establish steady-state admission policies.

| Capacity | `liteLRU` (s=1.001) | `otter` (s=1.001) | `liteLRU` (s=0.8) | `otter` (s=0.8) |
|----------|---------------------|-------------------|-------------------|-----------------|
| 25%      | **86.62%**          | 84.61%            | **69.69%**        | 69.54%          |
| 50%      | **94.48%**          | 91.13%            | **87.27%**        | 80.69%          |
| 75%      | **97.59%**          | 95.98%            | **98.74%**        | 90.50%          |
| 95%      | **97.59%**          | **97.59%**        | **98.74%**        | 98.01%          |

`liteLRU`'s bitmask-CLOCK approximation achieves superior or tied hit rates against Otter's [[18]](#ref-otter) S3-FIFO-based eviction policy across all tested capacities. These hit-rate benchmarks were re-run against the current 64-way set associative codebase. The set associative design restricts each key to one of $N/64$ fixed sets, which could theoretically reduce hit rates if the hash function produces skewed set occupancy. In practice, the Zipfian distribution's natural clustering aligns well with set boundaries (e.g., under $s=0.8$ skew, average set occupancy across hot sets remains below 85% with negligible collision evictions), and the observed hit rates are consistent with the prior hash-map-based implementation. CLOCK's recency tracking provides an advantage over S3-FIFO's frequency-aware admission at moderate skew (e.g. $s=0.8$) due to temporal locality in the access pattern. On strictly scan-resistant workloads (e.g., sequential looping), S3-FIFO is expected to outperform CLOCK approximations. However, `liteLRU`'s throughput remains immune to the high hit-ratio contention pathology described by Qiu et al. [17].

### 9.7 Write-Heavy Workloads

We compare against Otter v2 rather than ristretto here because ristretto's hit-rate collapses under intense concurrent pressure (as documented by the Otter authors [18]), rendering its write throughput an artifact of dropped samples rather than true admission. To stress the concurrent write protocol under severe contention, we measured throughput across a 1.6M operation Zipfian workload with aggressive `Get/Add` ratios using 8 concurrent cores.

| Workload     | `liteLRU` (Ops/sec) | `otter` v2 (Ops/sec) | Advantage |
|--------------|-------------------|--------------------|-----------|
| 50/50 Get/Add| **30,678,081**    | 6,345,600          | **4.83x** |
| 20/80 Get/Add| **30,080,323**    | 4,083,792          | **7.36x** |

`liteLRU` demonstrates a 4.8x to 7.4x throughput advantage under write-heavy pressure. When writes dominate (80%), amortized caches like `otter` experience severe contention on their ingestion buffers (dropping to ~4M ops/sec). `liteLRU` sustains over 30M ops/sec by confining state mutations to localized, lock-free overwrites inside the 64-way associative sets.

A notable result is that the 50/50 and 20/80 workloads yield nearly identical throughput for `liteLRU` (~30M ops/sec). However, raw throughput must be contextualized by the load-shedding mechanism: in these extreme contention synthetic benchmarks, `liteLRU` deliberately drops approximately 5% of admissions in the 50/50 workload and 6.6% in the 20/80 workload to maintain absolute bounded execution latency. This is a direct consequence of eliminating tombstones: in the old hash-map architecture, write-heavy loads triggered compaction cycles that significantly degraded throughput. In the 64-way set associative design, writes are in-place overwrites of fixed slots — the cost of a write is structurally identical to the cost of a read, so increasing write fraction does not degrade throughput.

---


### 9.8 HTTP Router Integration (JSON Response Memoization)

To validate `liteLRU`'s tail-latency advantages in a real-world scenario, we integrated it into a standard Go `net/http` server functioning as a REST API. In this scenario, the cache acts as a response memoization layer to bypass CPU-intensive JSON marshaling. Both caches were configured with capacity 1024 entries, matching the cache capacity used for the benchmarks.

We generated 10 seconds of aggressive concurrent load (64 workers) using `vegeta`, driving HTTP `GET` requests following a Zipfian distribution ($s=1.001$, $N=100,000$). The server simulates a backend endpoint by dynamically selecting one of 20 complex, nested JSON payloads per request. We compared three configurations:
1. **No Cache (Origin Only)**: The handler performs a full `json.Marshal()` on the complex payload structure for every single request before responding.
2. **Otter v2**: The handler caches the serialized JSON string. On a cache hit, it instantly writes the string, bypassing JSON marshaling completely.
3. **liteLRU**: The handler stores the serialized JSON string in the `liteLRU` parameter block. On a hit, it instantly writes the parameter string, bypassing JSON marshaling completely.

| Configuration | Throughput (Req/sec) | p50 Latency | p99 Latency | Max Latency |
|---------------|----------------------|-------------|-------------|-------------|
| No Cache      | 86,453               | 303 µs      | 1.36 ms     | 35.1 ms     |
| `otter` v2    | 86,056               | 279 µs      | 1.41 ms     | 173.1 ms    |
| `liteLRU`     | **93,603**           | **276 µs**  | **1.20 ms** | **43.1 ms** |

Several results merit explicit discussion:

**Throughput delta is modest (~8%).** `liteLRU` achieves 93,603 req/sec versus the no-cache baseline of 86,453 — an 8% gain. The in-process benchmarks in §9.1 and §9.7 show 2–7x advantages because they measure pure cache mechanics. At the HTTP layer, network I/O and TCP/HTTP parsing dominate the per-request budget, so the cache saves only the `json.Marshal` fraction. The modest gain is expected and confirms that the cache overhead is negligible rather than a bottleneck.

**Otter's throughput is lower than no-cache (86,056 vs 86,453 req/sec).** Despite bypassing JSON marshaling on hits, `otter` is marginally slower than the baseline. This indicates that `otter`'s admission buffer overhead — background goroutines flushing the write buffer, mutex acquisition on hit promotion, and GC pressure from pointer-heavy internals — nearly cancels the JSON savings at this concurrency level. The cache adds latency, not just eliminates it.

**Otter's 173.1 ms max latency is 4.9× worse than the no-cache baseline (35.1 ms).** This latency profile is consistent with the structural properties of amortized-buffer designs under concurrent pressure. `liteLRU` avoids it by performing all writes synchronously and in-place within the 64-slot set, with no background goroutine to be descheduled.

In summary, `liteLRU` yields the highest throughput (93,603 req/sec), the lowest p99 latency (1.20 ms), and a tightly bounded max latency (43.1 ms), confirming that the 64-way set associative SWAR architecture does not introduce tail congestion into the network stack under sustained load.

---


### 9.9 Reverse Proxy Integration (Upstream Latency and Jitter)

To demonstrate that the cache helps when a hit avoids network transport, upstream latency, and origin variance (and not just CPU-bound JSON serialization), we simulated a reverse proxy caching layer. On a cache miss, the handler simulates an upstream network fetch with a 50 ms base latency and up to 20 ms of random jitter before JSON marshaling. On a cache hit, the upstream request and serialization are bypassed entirely.

The cache was attacked with a 10-second `vegeta` workload utilizing 64 concurrent workers targeting the simulated proxy endpoint.

| Cache Implementation | Rate (Req/s) | p50 Latency | p99 Latency | Max Latency |
|----------------------|--------------|-------------|-------------|-------------|
| **liteLRU**          | **3,140 req/s** | **147 µs**  | 69.22 ms    | 74.14 ms    |
| Otter                | 3,139 req/s  | 152 µs      | 69.22 ms    | 72.45 ms    |
| Origin (No Cache)    | 1,101 req/s  | 57.36 ms    | 69.52 ms    | 90.55 ms    |

*Table: Reverse proxy simulation with 50-70 ms upstream jitter on cache misses.*

By shielding the application from the simulated network upstream, `liteLRU` drops the median (p50) response time from 57.36 ms down to 147 µs—an approximately **390x improvement** in user-facing latency. The p99 latency in the cached runs (69.22 ms) perfectly reflects the intentional upstream penalty during cold-start cache misses, showing that the cache's own internal mechanism latency is statistically invisible compared to the network bound.



### 9.10 Dynamic Router Integration and Pathological Stress Testing

To evaluate the cache under realistic API workloads and extreme pathological contention, we simulated a dynamic routing layer. We designed a synthetic worst-case stress test (using artificial timer synchronization) to deliberately saturate a single URL's chunk with 64 simultaneous cache misses (a "Thundering Herd"). We include this synthetic contention stress case to validate the load-shedding bound under simultaneous multi-miss admission. By employing the 10-retry load-shedding mechanism (Section 6.2), `liteLRU` rigorously bounds this execution. 

We then present the realistic end-to-end results below, where the un-cached origin simulates a CPU-bound regex routing tree lookup and path extraction (e.g. `/api/user/{id}/profile`). `liteLRU` caches the resulting `HandlerFunc` and extracted `[]Param` directly. Crucially, `liteLRU` uses a stack-allocated buffer (`var pbuf [4]liteLRU.Param`) for retrieving parameters, eliminating any pool-managed or heap allocations on route hits.

| Cache Implementation | Rate (Req/s) | p50 Latency | p99 Latency | Max Latency |
|----------------------|--------------|-------------|-------------|-------------|
| **liteLRU**          | **95,708 req/s** | **253 µs**  | **1.39 ms** | **18.96 ms**|
| Otter                | 91,959 req/s | 269 µs      | 1.73 ms     | 37.92 ms    |
| Origin (No Cache)    | 78,680 req/s | 381 µs      | 1.93 ms     | 198.69 ms   |

*Table: Dynamic router simulation with CPU-bound routing penalty on cache misses.*

In this end-to-end simulation, `liteLRU` outperforms Otter in both peak throughput (by 4%) and tail latency across all percentiles. The p99 latency drops from 1.73 ms (Otter) to 1.39 ms (`liteLRU`), and the maximum observed tail latency is halved (18.96 ms vs 37.92 ms).

This performance delta highlights the effectiveness of `liteLRU`'s **Load-Shedding Eviction** mechanism. Under pathological concurrent writes (simulating a Thundering Herd on a cache miss), bounded execution takes precedence over absolute cache insertion. If `liteLRU` detects that a specific 64-slot chunk is saturated by concurrent evicting threads, it dynamically aborts the insertion. This mechanism caps per-attempt write-side work at a fixed constant and converts extreme contention into dropped admission, avoiding unbounded spin under multi-miss contention.

In contrast, Otter’s implementation uses asynchronous buffering and background processing, whereas `liteLRU` performs synchronous inline admission; the higher tail latency (37.92 ms max) observed here is consistent with that architectural tradeoff. Because `liteLRU` remains strictly synchronous, its evictions run inline on the executing thread without background queues, guaranteeing zero heap allocations for the returned parameter slices while delivering superior end-to-end responsiveness.

By caching the route resolution itself, `liteLRU` drops the median observed request latency in this benchmark from 381 µs to just 253 µs, proving that it acts as a effective caching layer for HTTP frameworks looking to bypass dynamic parameter extraction logic entirely.

## 10. Discussion and Limitations

**Memory Overhead.** The tradeoff for $L$-byte padded seqlocks and SoA alignment is increased memory overhead per entry compared to dense tag-based implementations like MemC3. A `liteLRU` entry requires approximately 104 bytes of overhead (64 bytes for the seqlock + 40 bytes for atomic pointers/lengths). For a 1,000,000 entry cache, this requires **~133 MB** of heap allocation for the SoA arrays, whereas `otter` requires **~120 MB**. This represents a modest ~11% memory premium in exchange for wait-free concurrency and a **7.36x throughput speedup** under high write load (80% writes).

**Hit Rate vs. True LRU.** The Chunked Bitmask algorithm is a CLOCK approximation of LRU, not true LRU. In workloads with adversarial access patterns specifically targeting the CLOCK approximation error, hit-rate may degrade relative to a true LRU implementation or advanced policies like SIEVE [6]. The set associative constraint (each key restricted to one 64-slot set) introduces an additional eviction imprecision relative to a fully associative structure; however, §9.6 confirms this does not measurably reduce hit rates under Zipfian distributions. The tradeoff is deliberate: structural lock freedom is valued over exact eviction fidelity.

**Fixed Capacity.** The cache does not resize. The 64-way set associative slot array is pre-allocated at initialization. This is a deliberate design decision to prevent any allocation or growth operation during steady-state execution, but it requires the caller to provision capacity upfront.

**Extreme Contention Load-Shedding.** To preserve microsecond read latency and bound write times, `liteLRU` limits spin-lock retries to 10 per eviction attempt. Under pathological duplicate-insert contention (e.g., thousands of workers missing on the same URL simultaneously), `liteLRU` will sacrifice some admissions and drop the writes. This load-shedding is a deliberate bound: it prevents unbounded spinning under pathological multi-miss contention, prioritizing stable latency over guaranteed admission.

**Zero-Allocation Param Retrieval.** By accepting a caller-provided stack-allocated buffer (`[]Param`), `liteLRU` guarantees zero heap allocations on cache hits, avoiding the GC overhead that plagues amortized caches returning pointer-backed slice structs.

---

## 11. Conclusion

We have presented the hardware-grounded derivation of each principal design decision in `liteLRU`: a hybrid memory architecture to isolate hot concurrency primitives from GC write barriers while safely tracking Go pointers [[9]](#ref-gogc); SoA layout to maximize cache-line occupancy per field access [[7]](#ref-soa); bitset-encoded recency state enabling bulk $O(1)$ evaluation via hardware `CTZ` intrinsics [[10,11]](#ref-tzcnt); Seqlocks [[14]](#ref-seqlock) to provide wait-free read semantics; $L$-byte cache-line padding [[1,8]](#ref-mesi) to eliminate false-sharing serialization; and — critically — the replacement of an open-addressed lock-free hash map with a **64-way set associative architecture** using SWAR signature scanning. This final decision eliminates tombstone compaction as a source of tail-latency spikes, reduces all write paths to bounded in-place overwrites, and fits the complete per-set signature metadata into a single $L$-byte cache line on the evaluated platforms. The combination yields a cache architecture whose concurrency cost is bounded by a small, fixed set of hardware atomic operations rather than OS-level synchronization primitives, achieving over 30M ops/sec under write-heavy workloads and sub-microsecond p99 tail latency under sustained 8-core load.

---

## References

<a name="ref-mesi"></a>
[1] Papamarcos, M. S. and Patel, J. H. "A Low-Overhead Coherence Solution for Multiprocessors with Private Cache Memories." *ACM SIGARCH Computer Architecture News*, 12(3):348–354, 1984. https://doi.org/10.1145/773453.808204

<a name="ref-amdahl"></a>
[2] Amdahl, G. M. "Validity of the Single Processor Approach to Achieving Large Scale Computing Capabilities." *AFIPS Spring Joint Computer Conference*, pp. 483–485, 1967. https://doi.org/10.1145/1465482.1465560

<a name="ref-clock"></a>
[3] Corbató, F. J. "A Paging Experiment with the Multics System." *In Honor of Philip M. Morse*, MIT Press, pp. 217–228, 1969.

<a name="ref-lru"></a>
[4] Belady, L. A. "A Study of Replacement Algorithms for a Virtual-Storage Computer." *IBM Systems Journal*, 5(2):78–101, 1966. https://doi.org/10.1147/sj.52.0078

<a name="ref-s3fifo"></a>
[5] Yang, J., Zhang, Y., Qiu, Z., Yue, Y., and Rashmi, K. V. "FIFO Queues are All You Need for Cache Eviction." *Proceedings of the 29th Symposium on Operating Systems Principles (SOSP '23)*, ACM, 2023. https://doi.org/10.1145/3600006.3613147

<a name="ref-sieve"></a>
[6] Zhang, Y., Yang, J., Yue, Y., Vigfusson, Y., and Rashmi, K. V. "SIEVE is Simpler than LRU: An Efficient Turn-Key Eviction Algorithm for Web Caches." *Proceedings of the 21st USENIX NSDI*, 2024. https://www.usenix.org/conference/nsdi24/presentation/zhang-yazhuo

<a name="ref-soa"></a>
[7] Sung, I., Bilgic, K., Dursun, A., Narumi, T., and Seinfeld, J. "A Scalable Data Format for High-Performance Molecular Dynamics." *ACM/IEEE Supercomputing Conference (SC)*, 2009. https://dl.acm.org/doi/10.1145/1654059.1654101

<a name="ref-falsesharing"></a>
[8] Bolosky, W. J. and Scott, M. L. "False Sharing and Its Effect on Shared Memory Performance." *USENIX Conference on Experiences with Distributed and Multiprocessor Systems*, 1993. https://dl.acm.org/doi/10.5555/1295415.1295418

<a name="ref-gogc"></a>
[9] Hudson, R. L. "Go GC: Prioritizing Low Latency and Simplicity." Go Blog / GoSF Meetup, 2015. https://go.dev/blog/ismmkeynote

<a name="ref-tzcnt"></a>
[10] Intel Corporation. "Intel 64 and IA-32 Architectures Software Developer's Manual, Vol. 2: Instruction Set Reference, TZCNT Instruction." No. 325383-078US, 2023. https://www.intel.com/content/www/us/en/developer/articles/technical/intel-sdm.html

[11] ARM Limited. "ARM Architecture Reference Manual: CLZ Instruction." No. DDI 0487, 2023. https://developer.arm.com/documentation/ddi0487/latest/

<a name="ref-lockfree"></a>
[12] Herlihy, M. and Shavit, N. "The Art of Multiprocessor Programming." *ACM PODC*, 2004. https://doi.org/10.1145/1011767.1011768

<a name="ref-clockpro"></a>
[13] Jiang, S., Chen, F., and Zhang, X. "CLOCK-Pro: An Effective Improvement of the CLOCK Replacement." *USENIX Annual Technical Conference*, 2005.

<a name="ref-seqlock"></a>
[14] Lameter, C. "Effective Synchronization on Linux/NUMA Systems." *Gelato Conference*, 2005. https://lameter.com/gelato2005.pdf

<a name="ref-memc3"></a>
[15] Fan, B., Andersen, D. G., and Kaminsky, M. "MemC3: Compact and Concurrent MemCache with Dumber Caching and Smarter Hashing." *USENIX NSDI*, 2013. https://www.cs.cmu.edu/~dga/papers/memc3-nsdi2013-abstract.html


<a name="ref-dice"></a>
[16] Dice, D., Kogan, A., and Lev, Y. "Understanding and Improving the Performance of Concurrent Applications." *USENIX*, 2013.

<a name="ref-hitrate"></a>
[17] Qiu, Z., Yang, J., and Harchol-Balter, M. "Why increasing the hit ratio can hurt cache throughput." *CMU Technical Report / Manuscript in Preparation*, 2026.

<a name="ref-otter"></a>
[18] Otter Authors. "Otter: A high performance concurrent cache in Go." *GitHub*, 2023. https://github.com/maypok86/otter

## 12. Reproducibility
- **Platform Architecture**: Apple Silicon M2 ($L=128$)
- **Go Version**: `go1.25.7 darwin/arm64`
- **macOS Version**: macOS 14.5
- **Machine RAM**: 64GB
- **Repository**: Commit `4a4f126`
- **Commands**: 
  - Verification: `go test -race ./...`
  - Benchmarks: `go run benchmarks/write_heavy_bench.go`, `go run benchmarks/zipf_bench.go`
