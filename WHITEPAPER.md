<div align="center">

# liteLRU: A Coherence-Conscious, Contention-Bounded Cache for Managed Runtimes via Chunked Bitmasks

**Author:** xDarkicex  
**Affiliations:** libravdb · bitdev · LibraVDB, INC.  
**Contact:** git@libravdb.com  
**Repository:** https://github.com/xDarkicex/liteLRU

---


</div>

## Abstract

Concurrent approximate LRU structures often serialize cores on shared metadata. We investigate whether a bounded-probe, 64-way set-associative cache combining bit-parallel CLOCK recency tracking, padded seqlocks, and contention-aware admission load-shedding can deliver predictable concurrency costs without centralized locking in a garbage-collected runtime. We present `liteLRU`, a fixed-capacity cache in Go whose synchronization metadata (seqlocks, bitmasks, and SWAR signatures) resides in off-heap `mmap` memory while pointer-bearing payloads remain on the Go heap behind single-word `atomic.Pointer` wrappers to immutable values. Reads are bounded-probe lock-free and zero-allocation on hits when supplied a caller buffer; writes are best-effort upserts with contention-bounded admission attempts ($R=10$ retries). Across microbenchmarks and end-to-end HTTP workloads on an 8-core Apple M2 (24 GB RAM), the hardened race-free `liteLRU` sustains ~17–23M ops/s under 80/20 and write-heavy Zipf workloads while shedding 2.15–3.45% of write attempts under peak contention, and maintains sub-microsecond p99 latency on the baseline harness (§9.1, §9.7).

---

## 1. Background: The Hardware Model

To reason about concurrent cache design, we first establish a precise model of the hardware execution environment.

### 1.1 The CPU Cache Hierarchy

A modern multi-core processor maintains a private L1 cache (typically 32–64 KiB, 4–5 cycle latency), a private L2 cache (256 KiB – 1 MiB, ~12 cycle latency), and a shared L3 cache (several MiB, ~40 cycle latency). Main memory sits at approximately 200–300 cycles. The performance implication is stark: a single L1 cache miss costs roughly 40× more than an L1 hit.

The fundamental unit of coherency is the **cache line**, dependent on the target platform architecture (e.g., 64 bytes on x86_64, 128 bytes on Apple Silicon). The CPU never fetches individual bytes; it fetches and writes back entire coherence lines.

### 1.2 Coherence and False Sharing

We use MESI terminology as an explanatory model for cache-coherence traffic on common shared-memory multiprocessors [[1]](#ref-mesi). The implementation depends only on the more general property that writes to a shared coherence granule can invalidate or otherwise require ownership transfer from copies held by other cores. Exact protocol states and instruction costs are microarchitecture-specific.

The critical consequence is **false sharing** [[8]](#ref-falsesharing): if two independent variables, accessed concurrently by two different cores, reside on the same $L$-byte coherence line, a write by one core forces an ownership transfer on the other core's copy, even though the second core is accessing a logically unrelated variable. This is a pure hardware serialization artifact with no algorithmic solution other than spatial separation.

### 1.3 Atomics and Memory Ordering Cost

Atomic operations (`CAS`, `fetch-and-add`) on a shared memory location generally require exclusive ownership of the coherence line.

Under high concurrency, a single globally contested atomic (e.g., a global clock-hand pointer) causes every participating core to serialize. As core count $N$ grows, coherence queues become a bottleneck. The lock-free design methodology necessary to escape this bound is treated extensively by Herlihy and Shavit [[12]](#ref-lockfree).

---

## 1.4 Research Question and Contributions

CLOCK, set associativity, compact signatures, and seqlocks are well-established primitives; `liteLRU` does not propose a new eviction policy. Instead, we investigate a specific systems-engineering hypothesis for managed, garbage-collected runtimes:

> *Can a bounded-probe, 64-way set-associative cache using bit-parallel recency tracking, padded seqlocks, and contention-aware admission provide predictable concurrency costs without centralized locking in Go?*

We separate the resulting design into three orthogonal guarantees—**Safety** (no torn payloads, invalid pointer exposure, or concurrent unsynchronized mutation), **Progress** (bounded-probe reads and retry-bounded admission attempts), and **Performance** (reduced coherence traffic and predictable tail latency)—and contribute:

* **Contention-Bounded Set-Associative Mechanism:** A 64-way set associative architecture utilizing chunked bitmasks and hardware `CTZ`, eliminating global clock hands and open-addressed tombstone maintenance.
* **Hybrid Memory Architecture:** Off-heap `mmap` allocation for pointer-free lock-free synchronization state (bitmasks, signatures, seqlocks) combined with on-heap `atomic.Pointer` SoA arrays to immutable values so the Go GC safely traces live strings, closures, and parameter slices.
* **Padded Seqlocks:** A cache-line-aligned structural layout using monotonic 64-bit seqlocks that yields bounded-probe lock-free reads and eliminates false sharing across per-slot sequence counters.
* **Load-Shedding Admission Protocol:** A best-effort upsert protocol that caps per-admission claim retries to $R=10$ CAS attempts; under sustained contention, admission or redundant updates may be dropped rather than spinning unboundedly.

The rest of this paper is structured as follows: Section 2 details the hybrid memory architecture, Section 3 discusses related work, Section 4 outlines the set-associative design, and the remaining sections analyze the eviction algorithm, safety, and performance.

## 2. Hybrid Memory Architecture and GC Isolation

**Scanner Heap Pressure.** Objects that escape to the heap are scanned during the mark phase. A cache holding thousands of string values generates a large root set for the GC to scan, increasing mark phase duration proportionally.

`liteLRU` addresses this through two non-trivial architectural changes:

1. **64-Way Set Associativity over Hash Maps**: The prior open-addressed designs considered by `liteLRU` use tombstones or comparable reclamation mechanisms to preserve concurrent probe sequences. Their maintenance and reclamation costs complicate predictable deletion behavior under sustained eviction. `liteLRU` instead overwrites a victim in place within a fixed set. An incoming route hash is mathematically mapped to a specific 64-slot bucket (a "set").
2. **SWAR Signature Scanning**: To instantly find a key within the 64-slot set without traversing arrays of strings, `liteLRU` maintains a `uint64` signature word per 8 slots. Each byte in the word represents an 8-bit hash signature. SWAR-style scalar bitwise operations test eight 8-bit signatures per machine word, reducing the number of signature-word loads and branchy per-slot comparisons before full-key verification. Lookups are bounded by fixed 64-way associativity. An 8-bit signature carries a 1/256 per-slot false-positive probability. Across 8 slots per word, the expected false-positive rate per word is approximately $1 - (1 - 1/256)^8 \approx 3\%$. Any SWAR match is confirmed by a full string comparison before the entry is returned; false positives incur one extra comparison, not an incorrect response. The signature array occupies 64 bytes. It is compact relative to an array of string pointers, but because Go does not expose struct alignment primitives, the array may straddle two coherence lines in memory. Regardless, the entire signature sweep requires at most two $L$-byte cache line fetches.

To achieve this without sacrificing safety, `liteLRU` employs a **hybrid memory architecture**. High-frequency concurrency primitives (bitmasks, seqlocks) are allocated purely off-heap via anonymous `mmap` to eliminate GC tracing and write barriers entirely. Conversely, data arrays containing Go pointers (keys, values, and handler functions) remain on the standard Go heap. This separation ensures the GC can still safely track and collect dynamic strings or closures, completely avoiding dangling-pointer hazards while keeping the hot eviction path invisible to the GC.

---

## 3. Related Work

The problem of concurrent cache design generally branches into two orthogonal domains: optimizing the eviction *policy* for higher hit rates, and optimizing the concurrent *mechanism* for lower CPU overhead. `liteLRU` is strictly a contribution to the latter.

**Concurrent CLOCK Mechanisms.** The closest published system to `liteLRU` is MemC3 [[16]](#ref-memc3), which optimized Memcached by introducing a concurrent CLOCK with 1-bit recency tags and optimistic locking to remove mutexes from the read path. `liteLRU` builds on this foundation by applying it to the Go runtime (off-heap `mmap`, zero-allocation SoA), but replaces MemC3's sequential bit-checks with bulk 64-bit bitmask evaluations via hardware `CTZ` intrinsics, drastically reducing eviction scan latency.

**Advanced Eviction Policies.** Recent systems like CAR, CLOCK-Pro [[13]](#ref-clockpro), S3-FIFO [[5]](#ref-s3fifo), and SIEVE [[6]](#ref-sieve) demonstrate that scan-resistant eviction policies can outperform strict LRU on web workloads. These policy innovations are generally orthogonal to the mechanism: a SIEVE or S3-FIFO algorithm still requires a concurrent implementation strategy to scale across cores without locking.

**Production Go Caches.** Existing Go caches like `freecache`, `bigcache`, and `ristretto` achieve high throughput via sharding and background eviction goroutines. `liteLRU` takes a different approach: rather than amortizing eviction cost over background threads or coarse-grained shard locks, `liteLRU` eliminates the locking overhead entirely via bitmask mathematics and seqlocks, enabling bounded-probe lock-free synchronous reads and inline contention-bounded writes.

---

## 4. Why Structure of Arrays over Array of Structs

*Note on Set Associativity:* Software caches traditionally use open-addressed hash maps, whereas hardware L1/L2 caches use $N$-way set associativity. `liteLRU` applies this hardware-style set associative design in software to bypass the global locking overhead of hash maps entirely.

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

When a `Get` operation scans `methods[i]` and `methods[i+1]`, both values reside in the same $L$-byte cache line (a `string` header is 16 bytes, so 4 string headers per cache line). Accessing 4 consecutive methods costs 1 cache-line fetch instead of 4 (since the target coherence-line size $L$ is at least 64 bytes).

Formally, let $W_f$ be the width of field $f$ and $L$ be the target platform's coherence-line size. For AoS, the number of cache lines fetched to access field $f$ of $n$ consecutive entries depends heavily on padding and struct size. If $\text{sizeof}(\text{Entry}) \le L$ and the compiler naturally packs them, we approximate the cost as fetching one new cache line per entry:

$$\text{Lines}_{AoS}(n, f) \approx n$$

For SoA, values are packed contiguously. Accounting for an arbitrary unaligned start offset $a$ within a coherence line ($0 \le a < L$):

$$\text{Lines}_{SoA}(n, f) = \left\lceil \frac{a + n \cdot W_f}{L} \right\rceil$$

Since $W_f \ll \text{sizeof}(\text{Entry})$, we have $\text{Lines}_{SoA}(n, f) \ll \text{Lines}_{AoS}(n, f)$ for sufficiently large $n > 1$.

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

Standard CLOCK still maintains a global clock-hand pointer — an atomic integer indicating the current sweep position. Every eviction increments this counter and examines a single entry. Under high concurrency, all evicting threads contend on this single atomic, creating repeated coherence ownership transfers under contention.

The key observation driving `liteLRU`'s design: if we partition the cache into chunks of exactly 64 entries, the valid state and access state of all 64 entries in a chunk can be represented as two 64-bit integers:

- $V_k \in \{0,1\}^{64}$: bit $i = 1$ iff slot $i$ holds a valid entry.
- $A_k \in \{0,1\}^{64}$: bit $i = 1$ iff slot $i$ has been accessed since last sweep.
- $W_k \in \{0,1\}^{64}$: bit $i = 1$ iff a writer currently owns slot $i$ for mutation.

A slot is an eviction candidate if it is empty ($V_k[i] = 0$) or valid but unaccessed ($V_k[i] = 1 \land A_k[i] = 0$), provided it is not currently owned by a writer ($W_k[i] = 0$). The set of all candidates across all 64 slots is computed in a single bitwise expression:

$$C_k = \neg (V_k \land A_k) \land \neg W_k$$

The index of the first candidate is then extracted using the hardware `CTZ` (Count Trailing Zeros) instruction [[10,11]](#ref-tzcnt). It is crucial to handle platform-specific zero behavior: on x86_64, `TZCNT` returns 64 when the input is zero, whereas on ARM64, `RBIT` + `CLZ` requires an explicit branch or zero-check. When a non-zero candidate mask exists:

$$i = \text{CTZ}(C_k)$$

The bitmask representation drastically reduces the cost of scanning. A core seeking eviction performs an atomic load of $A_k$. If $C_k = 0$, either all valid slots have been accessed or free candidates are currently locked in $W_k$. The core atomically applies a bulk wipe $A_k \leftarrow 0$. It is a known tradeoff of this bulk-clearing approach that bits set by concurrent readers microseconds prior will be wiped alongside stale bits; however, this mild loss of precision is accepted in exchange for eliminating $O(N)$ per-slot atomic instructions.

**Writer Progress and Claim Protocol.** 

The eviction search process executes as follows:
1. Extract candidate index $i = \text{CTZ}(C_k)$.
2. Execute an atomic Compare-And-Swap (CAS) to set $W_k[i] \leftarrow 1$.
3. If the CAS fails (another writer claimed it), restart at step 1 for the same chunk.
4. If $W_k[i] = 1$ is successfully claimed, proceed with the in-place 64-way set associative write protocol defined in §6.

To guarantee contention-bounded admission and bound the worst-case scenario where a chunk is pathologically saturated by concurrent writers (e.g., a Thundering Herd), `liteLRU` employs **Load-Shedding Eviction**. The `findVictim` loop attempts to claim a write-lock (`writing` bit) on an eviction candidate. If it fails to claim any slot after 10 retries, it returns a sentinel failure value (`0xFFFFFFFF`). The `Add` operation intercepts this sentinel and safely drops the cache admission. By sacrificing absolute admission under extreme contention, `liteLRU` bounds write-side work to a fixed number of CAS attempts; under sustained contention, admission may be dropped.

### 5.4 Eliminating the Global Clock Hand

Because `liteLRU` seeds the starting chunk index from the hash of the incoming route, $k_{start} = h(r) \bmod N$, there is no global clock-hand atomic. Each thread begins its eviction sweep at a deterministic but distributed starting position. Contention on a shared sweep counter is eliminated entirely. 

---

## 6. Algorithm and Correctness

### 6.1 State Definitions and Layout

The cache state is divided into global configuration, per-chunk metadata, and per-slot data:

* **Global:** Total capacity $C$ and number of chunks $N = C/64$.
* **Per-Chunk (off-heap `mmap`):** A `chunk` struct containing 64-bit masks $V_k$ (valid bits), $W_k$ (writing/ownership bits), $A_k$ (access/recency bits), and an array of 8 `uint64` SWAR signatures for fast scanning.
* **Per-Slot:** A `slotState` (off-heap `mmap`, padded to the target platform's $L$-byte coherence stride) containing the monotonic 64-bit seqlock $S_i$, alongside Structure of Arrays (SoA) slices on the Go heap (`[]atomic.Pointer[string]`, `[]atomic.Pointer[HandlerFunc]`, `[]atomic.Pointer[[]Param]`) pointing to immutable key and payload objects.

### 6.2 API Semantics, State Machine, and Control Flow

**Best-Effort Upsert and Duplicate-Key Semantics.** `Add(key, value)` is defined as a **best-effort set-associative upsert** rather than a globally serialized unique-key map insertion:
1. If an existing valid entry for `key` is found during the initial chunk scan, `Add` acquires exclusive slot ownership via $W_k[i]$ and updates its value in place under the seqlock. If $W_k[i]$ is already owned by another writer for that exact slot, `Add` sheds the redundant concurrent update (`drops++`) and returns.
2. If no matching entry is observed, `Add` claims an eviction victim in chunk $k$ via `findVictim(k)`. Under simultaneous cold-start misses on the same `key`, two writers holding *distinct* $W_k$ bits may transiently admit duplicate entries in two slots of chunk $k$. Because `Get` scans slots $0\dots 63$ in deterministic ascending order and sets $A_k$ only for the first clean match, `Get` returns the lowest-index valid entry while unaccessed duplicate slots are naturally reclaimed by CLOCK sweeps.

**Slot Publication State Machine.** Each slot transitions through three logical states governed by $(W_k[i], V_k[i], S_i)$:
* **INVALID** ($V_k[i] = 0$): Slot is empty or cleared prior to mutation; readers skip on $V_k[i] = 0$.
* **WRITING** ($W_k[i] = 1, S_i \equiv 1 \pmod 2$): Exclusively owned by one writer. `Add` clears $V_k[i] \leftarrow 0$ (on eviction/insert) and advances $S_i$ from $2g$ to $2g+1$ before publishing immutable `atomic.Pointer` fields, updating the SWAR signature byte, and setting $A_k[i] \leftarrow 1$.
* **VALID** ($V_k[i] = 1, S_i \equiv 0 \pmod 2$): Writer closes generation $S_i \leftarrow 2g+2$, sets $V_k[i] \leftarrow 1$ (linearization point for reader visibility), and releases $W_k[i] \leftarrow 0$. `Clear()` waits until $W_k = 0$ on the chunk, claims all 64 bits of $W_k$, clears $V_k/A_k/\text{sigs}$, and advances $S_i$ monotonically ($S_i + 1 \to S_i + 2$) rather than resetting to zero.

**Get(key)**
1. Hash the key to find chunk $k$ and 8-bit signature `sig8`.
2. Scan the 8 SWAR signature words (`i = 0..7`) using byte-wise comparison.
3. For any matching slot $i$ in ascending slot order, check $V_k[i] = 1$.
4. Load $S_{i, \text{start}}$. If odd, skip slot (active write).
5. Load the single-word `atomic.Pointer` key/value pointers and copy any `[]Param` into caller buffer `dst`.
6. Load $S_{i, \text{end}}$. If $S_{i, \text{start}} \neq S_{i, \text{end}}$, skip slot (concurrent write).
7. If $A_k[i] = 0$, atomically OR bit $i$ into $A_k$; increment hit counter and return.
8. If no slot matches cleanly, increment miss counter and return miss.

**Add(key, value)**
1. Hash the key to find chunk $k$ and `sig8`.
2. **Fast-path update:** Scan the 8 SWAR words for a valid slot matching `key` bracketed by matching even seqlock reads ($S_{i,\text{start}} = S_{i,\text{end}}$). If matched, attempt CAS on $W_k[i]: 0 \to 1$ (retrying up to $R=10$ times if unrelated bits in $W_k$ changed; dropping immediately if $W_k[i]=1$). Re-validate $(V_k[i]=1 \land \text{key matches})$ under $W_k[i]=1$. If valid, advance $S_i \to S_i+1$, publish immutable value pointers, set $A_k[i] \leftarrow 1$ if unset, advance $S_i \to S_i+2$, release $W_k[i] \leftarrow 0$, and return. (If evicted before claim, release $W_k[i]$ and continue scan.)
3. **Eviction / admission:** Otherwise run `findVictim(k)`. If it returns `0xFFFFFFFF`, increment drop counter and return.
4. Transition `VALID/EMPTY → INVALID → WRITING`: clear $V_k[i] \leftarrow 0$ and store $S_i \leftarrow S_i + 1$ (odd).
5. Publish immutable `atomic.Pointer` key (`methods`, `paths`) and value (`handlers`, `params`) fields.
6. Update the SWAR signature byte in `sigs` via CAS, and set $A_k[i] \leftarrow 1$ (second-chance protection for newly admitted entries).
7. Close generation ($S_i \leftarrow S_i + 2$, even), publish $V_k[i] \leftarrow 1$ (visibility linearization point), and release $W_k[i] \leftarrow 0$.

**findVictim(chunk k)**
1. Retry loop bounded by $R = 10$ failed iterations (`retries < 10`).
2. Load $V_k, A_k, W_k$ and compute $C_k = \neg(V_k \land A_k) \land \neg W_k$.
3. If $C_k \neq 0$, pick $i = \text{CTZ}(C_k)$ and attempt CAS $W_k[i]: 0 \to 1$ (the **ownership linearization point**). If CAS succeeds, return slot $i$; otherwise increment `retries` and retry if `retries < 10`.
4. If $C_k = 0$, atomically clear valid access bits via `chk.accessed.And(^validBits)`, increment `retries`, yield via `runtime.Gosched()` if $W_k \neq 0$, and loop if `retries < 10`.
5. If $R = 10$ retries are exhausted, return `0xFFFFFFFF` to shed admission load.

**Linearization Points and Stale Mask Snapshots.** Because `findVictim` loads $V_k$, $A_k$, and $W_k$ in separate atomic instructions before executing CAS on $W_k$, a reader may set $A_k[i] \leftarrow 1$ between loading $A_k$ and claiming $W_k[i]$. We distinguish **eviction-policy fidelity** from **ownership safety**: stale $A_k$ observations are explicitly permitted by approximate CLOCK and only affect victim preference, whereas exclusive slot ownership is linearized solely at the successful $W_k$ CAS. Once $W_k[i]=1$ is held, no other writer or `Clear()` can mutate slot $i$, $V_k[i]$, or $S_i$ until $W_k[i]$ is released.

### 6.3 Go Memory Model Safety

Seqlock validation alone does not make concurrent accesses to multi-word Go structs (like 16-byte `string` headers or 24-byte `[]Param` headers) race-free: loading `ptr` and `len` separately across a concurrent writer can tear and trigger out-of-bounds reads *before* $S_{i,\text{end}}$ is checked, and reusing a slice backing array in place creates a Go data race with concurrent readers.

`liteLRU` therefore combines off-heap synchronization metadata with **single-word `atomic.Pointer` publication of immutable heap values**:
1. Every concurrently read key/value field (`methods`, `paths`, `handlers`, `params`) is a single `atomic.Pointer` pointing to an immutable object (`string`, `HandlerFunc`, or freshly copied `[]Param`) that is never mutated after publication.
2. Writers to any slot $i$ are strictly serialized by $W_k[i]$ (`Add` fast-path updates, `Add` evictions, and `Clear` all acquire $W_k[i]=1$ before touching $V_k[i]$, $S_i$, or payload pointers).
3. The sequence counter $S_i$ is a monotonic `atomic.Uint64` (never reset on `Clear`) padded to $L$ bytes and never copied after initialization.
4. All synchronization primitives rely on Go's documented sequentially consistent `sync/atomic` total order [[15]](#ref-gomem).

### 6.4 Correctness Theorems

**Theorem 1 (Bounded-Probe Lock-Free Reads).** *Any thread executing `Get` terminates in a bounded number of metadata probes (at most 8 SWAR words and 64 slots; key comparison cost is $O(|\text{key}|)$), regardless of the state or execution speed of concurrent writers.*

*Proof:* `Get` performs no unbounded loops and never retries on seqlock conflict. It evaluates at most 8 SWAR words and 64 slot indices; for each matching signature byte it executes bounded atomic loads ($V_k$, $S_{i,\text{start}}$, single-word payload pointers, $S_{i,\text{end}}$) and at most one key comparison before either returning a hit (with one optional atomic `Or` on $A_k$) or advancing to the next slot and returning miss. $\square$

**Theorem 2 (Bounded Admission Attempt Complexity).** *Any thread executing `Add` executes at most $R = 10$ claim retries in `findVictim` (and at most $R = 10$ $W_k$-claim retries per matched fast-path slot), bounding total admission-attempt work as $T_{\mathrm{admission}} \le R \cdot T_{\mathrm{attempt}} + T_{\mathrm{publication}}$.*

*Proof:* In `findVictim`, every failed `W_k` CAS and every `A_k`-clearing pass (`chk.accessed.And(^validBits)`) increments the same retry counter `retries`, returning `0xFFFFFFFF` once `retries >= 10` ($N_{\mathrm{CAS,claim}} \le R = 10$). Similarly, the fast-path `W_k[i]` claim loop caps retries at $R=10$ on unrelated-slot interference and drops immediately if $W_k[i]=1$. This establishes a hard bound on contention-retry work rather than a deterministic wall-clock latency bound (OS scheduling, memory allocation, and post-claim lock-free bit/byte CAS updates on `chk.sigs` lie outside $R$). $\square$

**Theorem 3 (Safety Invariant).** *No two writers may simultaneously mutate the same slot, and an accepted read in `Get` observes a complete, untorn key/value snapshot from a single publication generation.*

*Proof:* Mutual exclusion among writers (`Add` fast path, `Add` eviction, and `Clear`) is enforced by $W_k[i]$: every writer acquires $W_k[i]: 0 \to 1$ via CAS (or all 64 bits in `Clear`) before modifying $V_k[i]$, $S_i$, or slot pointers, and holds $W_k[i]=1$ until the write completes. Under Go's sequentially consistent atomic order, writer generation $g$ stores all immutable `atomic.Pointer` fields while $S_i = 2g+1$ (odd) and only then stores $S_i \leftarrow 2g+2$ (even) before setting $V_k[i] \leftarrow 1$. If a reader loads $S_{i,\text{start}} = S_{i,\text{end}} = 2g+2$ (even), the monotonic `uint64` counter implies all payload pointer loads occurred strictly after writer $g$'s `S_i ← 2g+2` store and strictly before writer $(g+1)$'s `S_i ← 2g+3` store (assuming fewer than $2^{63}$ complete writes to one slot during a single read—unreachable in practice). Because each published `*string`, `*HandlerFunc`, and `*[]Param` target is immutable after publication, the reader never observes a torn header or concurrent backing-array mutation even if a concurrent writer begins generation $g+1$ before $S_{i,\text{end}}$ is checked. $\square$

## 7. Cache-Line Padding to Eliminate False Sharing

A naive implementation allocates seqlocks as a contiguous slice `[]atomic.Uint32`. Each `uint32` is 4 bytes, placing multiple seqlocks per cache line. By the MESI coherency protocol [[1]](#ref-mesi), a concurrent write to slot $j$ invalidates the line holding multiple neighboring seqlocks across all other cores — the false-sharing pathology described by Bolosky and Scott [[8]](#ref-falsesharing).

`slotState` metadata is allocated in a page-aligned anonymous `mmap` region. For each supported platform, `liteLRU` selects a slot stride equal to the platform's verified coherence-line size $L$ (64 bytes for x86_64, 128 bytes for Apple Silicon). Because the mapping base is page-aligned and each slot has stride $L$, distinct slot states begin on distinct coherence-line boundaries.

**Padding Theorem.** On a platform with coherence-line size $L$, if the base of the slot-state region is aligned to $L$ and each slot state has stride $L$, distinct slots occupy distinct coherence lines.

*Proof.* Let $\text{addr}(S_i)$ denote the base address of the padded slot state for slot $i$. We define:

```go
// L is the verified coherence-line size for the target platform via build tags.
type slotState struct {
    seq atomic.Uint64 // 8 bytes
    _   [L - 8]byte   // L-8 bytes padding
}
```

Because the `mmap` allocation guarantees $\text{addr}(S_0) \pmod L = 0$ (page alignment implies $L$ alignment for $L \le 4096$), and $\text{addr}(S_{i+1}) = \text{addr}(S_i) + L$, adjacent entries reside on disjoint, non-overlapping $L$-byte aligned cache lines. A write to $S_i$ can require coherence activity for its own line but does not require ownership transfer for a distinct slot-state line. False sharing is structurally impossible. $\square$

It is important to distinguish **metadata footprint** from **coherence traffic** for the 64-bit bitmasks ($V_k$, $A_k$, $W_k$). Packing 64 slots into one `uint64` reduces metadata word count by 64× compared to per-slot flags, but concentrates independent slot mutations within the same chunk onto shared coherence lines. `liteLRU` mitigates this concentration in two ways: `Get` reads `chk.accessed` first and only issues an atomic `Or` when the bit is unset, and `Add` bounds retry loops on shared chunk words to $R=10$.

The same argument applies to the statistics counters. Aggregating hits and misses into two global `atomic.Int64` values would cause every `Get` on every core to contend on two shared cache lines. We instead shard into 64 independent `statStripe` structs, each padded to the target platform's $L$-byte coherence stride, selected by `hash & 63`. Writes are distributed across 64 padded stripes, reducing—rather than eliminating—counter contention.

---

## 8. Benchmark Methodology and Artifacts

Standard Go micro-benchmarks use `b.RunParallel`, which distributes `b.N` iterations across goroutines via `pb.Next()`. While `b.RunParallel` amortizes synchronization over per-goroutine iteration batches rather than executing a shared atomic fetch-and-add on every single call, it still adds harness overhead and aggregates only mean `ns/op` without latency percentiles. Running `BenchmarkParallelLRUCache` on the fixed codebase (`80% Get / 20% Add`, 1024 capacity) yields **79.94 ns/op** (1 core), **50.79 ns/op** (2 cores), **29.36 ns/op** (4 cores), and **34.66 ns/op** (8 cores, reflecting Apple M2's 4 performance + 4 efficiency cores) with **0 allocs/op**.

Our custom latency and throughput harnesses complement `b.RunParallel` by running tight worker loops over pre-allocated key arrays with no shared iteration counter and recording per-operation or sampled `time.Now()` distributions directly.

---

## 9. Evaluation

**Platform:** Apple M2, 8-core ARM64, macOS. Go 1.25.7. Cache capacity 1024 entries.

### 9.1 Baseline Throughput and Tail Latency

**Workload:** 8 concurrent workers, $1.6 \times 10^6$ total operations, 80% `Get` / 20% `Add`, paths drawn from a pool of 1000 distinct routes (`benchmarks/baseline_bench.go`). Measured on the hardened race-free implementation against `otter` (`v1.2.4`) and a `Mutex LRU`:

| Cache Implementation      | Ops/sec    | p50 Latency | p99 Latency |
|---------------------------|-----------:|------------:|------------:|
| `liteLRU`                 | 22,862,450 | 250 ns      | 834 ns      |
| `otter` (`v1.2.4`)        | 17,239,212 | 292 ns      | 49.08 µs    |
| `Mutex LRU` (Naive)       |  9,416,272 | 208 ns      | 27.38 µs    |

*Note: Latency percentiles in this table were sampled at a 1% rate (to minimize the observer effect on RFO traffic) to preserve realistic throughput levels during measurement.*

`liteLRU` demonstrates an $\approx$2.43x throughput advantage over the naive Mutex LRU and a 1.33x advantage over `otter` (`v1.2.4`) after adding per-slot $W_k$ serialization on fast-path updates and immutable single-word pointer publication. Furthermore, `liteLRU` preserves sub-microsecond p99 tail latency (834 ns) compared to 49.08 µs for `otter` and 27.38 µs for `Mutex LRU`. `otter` absorbs common-path operations into background buffers, which is consistent with its higher tail latency; isolating individual internal causes would require profiling. `liteLRU` instead performs synchronous, bounded writes into the 64-way set associative slots.

### 9.2 Throughput Scaling Under Contention

To stress lock-free scaling under skewed access patterns, we scale a 1.6M operation **Zipfian** workload ($s=1.001$, $N=100{,}000$) across 1 to 8 concurrent cores. Unlike §9.1's uniform distribution, the Zipfian skew produces a hotter working set and greater eviction pressure, making this a distinct but complementary benchmark.

| Cores | Ops/sec       | Scaling Factor |
|------:|--------------|----------------|
| 1     | 6,078,793    | 1.00×          |
| 2     | 10,899,600   | 1.79×          |
| 4     | 18,972,613   | 3.12×          |
| 8     | 20,303,656   | 3.34×          |

The sub-linear scaling from 4 to 8 cores is expected: Scaling is sub-linear from four to eight workers, consistent with increasing contention and shared-resource pressure; this experiment does not isolate their individual contributions.

### 9.3 Latency Percentiles (ARM64)

Measured with an inherent ~40ns `time.Now()` overhead per sample:

| Percentile | Measured  | Estimated (overhead removed) |
|:----------:|----------:|-----------------------------:|
| p50        | 208 ns    | ~168 ns                      |
| p99        | 1,083 ns  | ~1,043 ns                    |
| p99.9      | 1,334 ns  | ~1,294 ns                    |
| Max        | ~6.3 ms   | —                            |

The observed maximum is consistent with OS scheduling jitter on at least one of the eight goroutines; the experiment does not isolate its cause, not cache mechanics.

### 9.4 Ablation Study

To isolate the contribution of each architectural decision in `liteLRU`'s **64-way set associative SWAR design**, we benchmarked the full implementation against three stripped-down variants under the identical 8-core 80/20 uniform workload. The ablation variants target the three novel mechanisms in the new architecture:

- **No Padding**: Removed the target-platform $L$-byte padding from seqlocks and stripe counters, inducing false sharing across concurrent accessors.
- **No SWAR Signatures**: Replaced the SWAR signature scan with a naive 64-iteration `for` loop performing full string comparisons on each slot.
- **No Set Associativity (Map+Mutex)**: Replaced the 64-way set associative slot array with a standard Go `map` protected by a `sync.RWMutex`. This represents a **combined penalty**, stripping away both lock-free associativity and SWAR scanning, restoring the lock-based eviction path the new architecture was designed to eliminate.

| Variant                        | Ops/sec    | Throughput Loss |
|--------------------------------|-----------:|----------------:|
| `liteLRU` (Full)*              | 32,024,792 | —               |
| No SWAR Signatures             | 29,034,906 | -9%             |
| No Padding (False Share)       | 19,407,171 | -39%            |
| No Set Associativity (Map+Mutex)|  4,280,582 | -87%            |

The ablation confirms that while SWAR hardware acceleration provides a measurable 9% optimization, the two structural requirements for scaling are cache-line isolation (-39% without padding) and elimination of the centralized mutex-protected map (-87%). The -87% result quantifies the combined cost of Go's `sync.RWMutex` overhead and the single-contention-point map: the 64-way set associative design avoids both by confining all writes to a localized slot via bounded-attempt CAS within the pre-hashed set.

*\*Note: The ablation runs use a distinct benchmark harness measuring absolute hardware limits without the 1% tracking overhead of the p99 latency evaluation, accounting for the higher ops/sec baseline relative to §9.1.*

### 9.5 x86_64 Smoke Test

To verify behavioral consistency across architectures, we evaluated `liteLRU` on an x86_64 Docker container instance. The x86_64 measurement is a behavioral smoke test only; its virtualized environment and different microarchitecture do not support a direct cross-platform performance comparison.

| Architecture | Cores | Ops/sec (x86_64) |
|--------------|------:|-----------------:|
| x86\_64       | 8     | 13,155,469       |

*Note: The x86_64 evaluation was executed within a virtualized Docker Desktop environment. Because hypervisor layers introduce non-deterministic scheduling jitter and distort cache-coherence traffic (such as Read-For-Ownership loops), these metrics are presented strictly as a functional and behavioral smoke test to verify cross-platform correctness of the atomic bitmask and TZCNT mechanics, rather than a bare-metal microarchitectural performance baseline.*

### 9.6 Zipfian Hit-Rate Evaluation

To evaluate eviction fidelity under realistic skewed access patterns, we benchmarked `liteLRU` against `otter` using a Zipfian distribution ($s=1.001$, $N=100,000$ working set) with 1.6M total operations across varying cache capacities. Both caches were given a 20% warmup phase to establish steady-state admission policies, using the same capacity definition (entry count) and key space. Values are the median of 10 runs.

| Capacity | `liteLRU` (s=1.001) | `otter` (s=1.001) | `liteLRU` (s=0.8) | `otter` (s=0.8) |
|----------|---------------------|-------------------|-------------------|-----------------|
| 25%      | **86.62%**          | 84.61%            | **69.69%**        | 69.54%          |
| 50%      | **94.48%**          | 91.13%            | **87.27%**        | 80.69%          |
| 75%      | **97.59%**          | 95.98%            | **98.74%**        | 90.50%          |
| 95%      | **97.59%**          | **97.59%**        | **98.74%**        | 98.01%          |

`liteLRU`'s bitmask-CLOCK approximation matches or exceeds Otter's [[18]](#ref-otter) S3-FIFO-based hit rates across all tested capacities. These hit-rate benchmarks were re-run against the current 64-way set associative codebase. The set associative design restricts each key to one of $N$ fixed sets, which could theoretically reduce hit rates if the hash function produces skewed set occupancy. In practice, the Zipfian distribution's natural clustering aligns well with set boundaries (e.g., under $s=0.8$ skew, average set occupancy across hot sets remains below 85% with negligible collision evictions), and the observed hit rates are consistent with the prior hash-map-based implementation. CLOCK's recency tracking provides an advantage over S3-FIFO's frequency-aware admission at moderate skew (e.g. $s=0.8$) due to temporal locality in the access pattern. On strictly scan-resistant workloads (e.g., sequential looping), S3-FIFO is expected to outperform CLOCK approximations. 

### 9.7 Write-Heavy Workloads

We omit Ristretto from this benchmark because, as documented in [18], it exhibits hit-rate degradation under extreme concurrent write pressure, making raw throughput comparisons an inaccurate reflection of actual admission. We therefore compare exclusively against `otter` (`v1.2.4`). To stress the concurrent write protocol under severe contention while quantifying useful (non-shed) work, we measured attempted throughput, effective (non-dropped) throughput, admission drop fraction, and resulting hit rate across a 1.6M operation Zipfian workload (`benchmarks/write_heavy_bench.go`) using 8 concurrent cores:

| Workload      | `liteLRU` Attempted | `liteLRU` Effective | Drop Rate (% of Adds) | Hit Rate | `otter` (`v1.2.4`) | Effective Advantage |
|---------------|--------------------:|--------------------:|----------------------:|---------:|-------------------:|--------------------:|
| 50/50 Get/Add | 18,335,438 ops/s    | 18,138,595 ops/s    | 2.15% (17,177)        | 99.54%   | 6,036,045 ops/s    | **3.01x**           |
| 20/80 Get/Add | 17,432,414 ops/s    | 16,951,497 ops/s    | 3.45% (44,140)        | 99.48%   | 3,756,957 ops/s    | **4.51x**           |

Even after accounting for dropped writes, `liteLRU` sustains 16.95M–18.14M **effective** ops/sec (3.01x–4.51x `otter`'s throughput) while maintaining a >99.4% hit rate. Under 80% writes, `liteLRU` sheds 3.45% of attempted `Add`s when multiple workers contend on the same hot slot's $W_k[i]$ ownership bit, trading redundant hot-key overwrites for bounded admission latency. Note that §§9.2–9.6 and §§9.8–9.10 below report the pre-hardening evaluation snapshot prior to adding fast-path $W_k$ locking and single-word `atomic.Pointer` wrappers; end-to-end HTTP runs (`vegeta`) with CPU/allocation/GC profiling remain future work.

---


### 9.8 HTTP Router Integration (JSON Response Memoization)

To validate `liteLRU`'s tail-latency advantages in a real-world scenario, we integrated it into a standard Go `net/http` server functioning as a REST API. In this scenario, the cache acts as a response memoization layer to bypass CPU-intensive JSON marshaling. Both caches were configured with capacity 1024 entries, matching the cache capacity used for the benchmarks.

We generated 10 seconds of aggressive concurrent load (64 workers) using `vegeta`, driving HTTP `GET` requests following a Zipfian distribution ($s=1.001$, $N=100,000$). The server simulates a backend endpoint by dynamically selecting one of 20 complex, nested JSON payloads per request. We compared three configurations:
1. **No Cache (Origin Only)**: The handler performs a full `json.Marshal()` on the complex payload structure for every single request before responding.
2. **Otter (`v1.2.4`)**: The handler caches the serialized JSON string. On a cache hit, it instantly writes the string, bypassing JSON marshaling completely.
3. **liteLRU**: The handler stores the serialized JSON string in the `liteLRU` parameter block. On a hit, it instantly writes the parameter string, bypassing JSON marshaling completely.

| Configuration | Throughput (Req/sec) | p50 Latency | p99 Latency | Max Latency |
|---------------|----------------------|-------------|-------------|-------------|
| No Cache      | 86,453               | 303 µs      | 1.36 ms     | 35.1 ms     |
| `otter` (`v1.2.4`) | 86,056          | 279 µs      | 1.41 ms     | 173.1 ms    |
| `liteLRU`     | **93,603**           | **276 µs**  | **1.20 ms** | **43.1 ms** |

Several results merit explicit discussion:

**Throughput delta is modest (~8%).** `liteLRU` achieves 93,603 req/sec versus the no-cache baseline of 86,453 — an 8% gain. The in-process benchmarks in §9.1 and §9.7 show 2–7x advantages because they measure pure cache mechanics. At the HTTP layer, network I/O and TCP/HTTP parsing dominate the per-request budget, so the cache saves only the `json.Marshal` fraction. The modest gain is expected and confirms that the cache overhead is negligible rather than a bottleneck.

**Otter's throughput is lower than no-cache (86,056 vs 86,453 req/sec).** Despite bypassing JSON marshaling on hits, `otter` is marginally slower than the baseline. This result is consistent with the overheads and tradeoffs of its asynchronous cache design, but attributing the difference to individual internal mechanisms would require profiling.

**Otter's 173.1 ms max latency is 4.9× worse than the no-cache baseline (35.1 ms).** This latency profile is consistent with the structural properties of amortized-buffer designs under concurrent pressure. `liteLRU` avoids it by performing all writes synchronously and in-place within the 64-slot set, with no background goroutine to be descheduled.

In summary, `liteLRU` yields the highest throughput (93,603 req/sec), the lowest p99 latency (1.20 ms), and an observed maximum latency of 43.1 ms in this 10-second run, confirming that the 64-way set associative SWAR architecture does not introduce tail congestion into the network stack under sustained load.

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

By caching the route resolution itself, `liteLRU` drops the median observed request latency in this benchmark from 381 µs to just 253 µs, proving that it acts as an effective caching layer for HTTP frameworks looking to bypass dynamic parameter extraction logic entirely.

### 9.11 Controlled Occupancy and Adversarial Stress Tests

To isolate the tradeoffs of fixed 64-way set associativity, 1-bit CLOCK, and $O(|\text{key}|)$ key comparisons (`benchmarks/adversarial_bench.go`, 1024-entry cache, 8 workers), we measured six targeted stress regimes:

| Experiment | Configuration | Observed Result |
|---|---|---|
| Single-set contention | 128 keys all hashed to chunk 0 (80/20 Get/Add) | 8,932,432 attempted / 8,902,938 effective ops/s; 1.65% Adds dropped; 32.95% hit rate |
| Associativity boundary | 64 vs. 65 keys in chunk 0 (1000 cyclic passes) | **100.00%** hit rate at 64 keys vs. **0.00%** at 65 keys |
| Occupancy sensitivity | 1% / 50% / 100% fill (80/20 Get/Add) | 50.32M / 26.62M / 11.31M ops/s (98.02% / 99.91% / 98.08% hit rate) |
| Sequential scan > capacity | 256 hot keys followed by 2048 scan keys | **0/256 (0.00%)** hot keys retained (CLOCK is not scan-resistant) |
| Single-key concurrent upsert | 8 workers, 1.6M `Add`s to `/single-hot` | 16.06M attempted / 5.50M effective ops/s; **65.76%** redundant writes shed |
| Key length scaling | 100% `Get`, 16 / 64 / 256 / 1024-byte keys | 52.78M / 23.01M / 12.00M / 3.51M ops/s |

These measurements make two limitations explicit: (1) a hot set exceeding 64 active keys experiences immediate cyclic eviction (64 vs. 65 boundary) even when other chunks are empty, and (2) 1-bit CLOCK is vulnerable to sequential scans exceeding cache capacity. Conversely, under pathological single-chunk or single-key write contention, load-shedding converts redundant updates into bounded drops without livelock, and key-length scaling confirms that metadata probes stay constant while key comparison scales with key length (Theorem 1).

## 10. Discussion and Limitations

**Memory Overhead.** The tradeoff for $L$-byte padded seqlocks and SoA alignment is increased memory overhead per entry compared to dense tag-based implementations like MemC3. On x86_64 ($L=64$), fixed per-slot arrays require 96 bytes/slot (64 bytes for `slotState` + 32 bytes for the four 8-byte `atomic.Pointer` SoA slices), totaling **~96 MB** per million slots before heap-allocated key/payload objects (`*string` wrappers, strings, and `[]Param`). On Apple Silicon ($L=128$), the 128-byte `slotState` padding brings fixed array overhead to 160 bytes/slot (**~160 MB** per million slots before chunk metadata and key/payload objects). Both represent a memory premium in exchange for bounded-probe lock-free concurrency, with `otter` requiring **~120 MB** per million entries.

**Hit Rate vs. True LRU.** The Chunked Bitmask algorithm is a CLOCK approximation of LRU, not true LRU. As demonstrated in §9.11, sequential scans larger than cache capacity evict hot entries, and workloads exceeding 64 hot keys within a single set suffer collision evictions even when global occupancy is low. The tradeoff is deliberate: structural lock freedom and bounded probes are valued over exact or scan-resistant eviction fidelity.

**Fixed Capacity.** The cache does not resize. The 64-way set associative slot array is pre-allocated at initialization. This is a deliberate design decision to prevent any allocation or growth operation during steady-state execution, but it requires the caller to provision capacity upfront.

**Extreme Contention Load-Shedding.** To bound write-side retry work, `liteLRU` limits admission claim retries in `findVictim` and fast-path `W_k` acquisition to $R=10$ attempts, and sheds redundant updates when a slot's `W_k` bit is already held. Under pathological single-key upsert contention (§9.11), `liteLRU` sheds up to ~66% of redundant writes to prevent unbounded spinning.

**Zero-Allocation Scope.** `Get` guarantees **0 heap allocations** (verified via `testing.AllocsPerRun`) on cache misses and on cache hits whenever the caller supplies a `dst` slice with `cap(dst) >= len(params)` (including `nil` when `len(params) == 0`). By contrast, `Add` allocates immutable `*string` / `*HandlerFunc` wrappers on miss admissions (and fresh `[]Param` copies when `len(params) > 0`) to preserve single-word race-free `atomic.Pointer` publication.

---

## 11. Conclusion

We have presented the hardware-grounded derivation of each principal design decision in `liteLRU`: a hybrid memory architecture isolating off-heap synchronization metadata (`slotState`, `chunk` bitmasks, and SWAR signatures) from Go GC barriers while safely tracking immutable heap payloads via single-word `atomic.Pointer` SoA slices [[9,15]](#ref-gogc); SoA layout to maximize cache-line occupancy per field access [[7]](#ref-soa); bitset-encoded recency state enabling bulk $O(1)$ candidate evaluation via hardware `CTZ` intrinsics [[10,11]](#ref-tzcnt); monotonic 64-bit Seqlocks [[14]](#ref-seqlock) with $L$-byte cache-line padding [[1,8]](#ref-mesi) to provide bounded-probe lock-free reads; and a **64-way set associative architecture** with SWAR signature scanning. Together with explicit $W_k$ ownership serialization and $R=10$ admission load-shedding, `liteLRU` delivers race-free concurrency, 16.95M–18.14M effective ops/sec under write-heavy Zipf workloads, and sub-µs p99 on the baseline harness (§9.1, §9.7).

---

## References

<a name="ref-mesi"></a>
[1] Papamarcos, M. S., & Patel, J. H. (1984). A low-overhead coherence solution for multiprocessors with private cache memories. *ISCA '84*.

<a name="ref-gomem"></a>
[15] The Go Memory Model. (2024). https://go.dev/ref/mem

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
[16] Fan, B., Andersen, D. G., and Kaminsky, M. "MemC3: Compact and Concurrent MemCache with Dumber Caching and Smarter Hashing." *USENIX NSDI*, 2013. https://www.cs.cmu.edu/~dga/papers/memc3-nsdi2013-abstract.html

<a name="ref-dice"></a>
[17] Dice, D., Kogan, A., and Lev, Y. "Understanding and Improving the Performance of Concurrent Applications." *USENIX*, 2013.




<a name="ref-otter"></a>
[18] Otter Authors. "Otter: A high performance concurrent cache in Go." *GitHub*, 2023. https://github.com/maypok86/otter

## 12. Reproducibility

* **Platform Architecture**: Apple Silicon M2 (8 cores, $L=128$)
* **Go Version**: `go1.25.7 darwin/arm64`
* **macOS Version**: macOS 14.5
* **Machine RAM**: 24GB (`hw.memsize = 25769803776`)
* **Baseline Snapshot**: Commit `f826423b006c4df62cb1fc1ddfcf76eb2e9b0682` (§§9.2–9.6, §§9.8–9.10; note that `benchmarks/ablation/` in the repository contains an earlier hash-map-based ablation harness)
* **Hardened Measurements**: §§8 (`BenchmarkParallelLRUCache`), 9.1, 9.7, and 9.11 report single-run measurements on the race-free `atomic.Pointer` / `W_k`-serialized implementation
* **Comparator**: `github.com/maypok86/otter v1.2.4`
* **Repetitions**: 10 independent runs for §§9.2–9.6; single-run verification for hardened §§8, 9.1, 9.7, 9.11
* **Environment**: `GOMAXPROCS=8`
- **Commands**: 
  - Verification: `go test -v -race ./...`
  - Parallel Go harness: `go test -run=^$ -bench=BenchmarkParallelLRUCache -benchmem -cpu=1,2,4,8 .`
  - Benchmarks (`cd benchmarks`): `go run baseline_bench.go`, `go run write_heavy_bench.go`, `go run adversarial_bench.go`, `go run zipf_bench.go`
