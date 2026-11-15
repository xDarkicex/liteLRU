<div align="center">

# liteLRU: A Coherence-Conscious, Contention-Bounded Cache for Managed Runtimes via Chunked Bitmasks

**Author:** xDarkicex  
**Affiliations:** libravdb · bitdev · LibraVDB, INC.  
**Contact:** git@libravdb.com  
**Repository:** https://github.com/xDarkicex/liteLRU

---


</div>

## Abstract

Concurrent approximate LRU structures often serialize cores on shared metadata. We investigate whether a bounded-probe, 64-way set-associative cache combining bit-parallel CLOCK recency tracking, padded seqlocks, and contention-aware admission load-shedding can deliver predictable concurrency costs without centralized locking in a garbage-collected runtime. We present `liteLRU`, a fixed-capacity cache in Go whose synchronization metadata (seqlocks, bitmasks, and SWAR signatures) resides in off-heap `mmap` memory while pointer-bearing payloads remain on the Go heap behind single-word `atomic.Pointer` wrappers to immutable values. Reads provide lock-free bounded-probe execution with zero heap allocations on hits when supplied a caller buffer; writes are best-effort, non-linearizable cache insertions/updates that cap per-slot ownership claim retries at $R=10$ (leaving post-claim signature-word CAS retries outside $R$). Across 10-run hardened evaluations on an 8-core Apple M2 (24 GB RAM), `liteLRU` sustains 21.60M–27.97M ops/s under 80/20 workloads (1.38× `otter v1.2.4` and 3.02× `Mutex LRU` on the uniform baseline, with 1.69 µs sampled p99), trails `otter` (`33.64M` vs. `94.27M`) under ultra-read-dominated insert-on-miss workloads (99.94% hits), and sustains 19.20M–21.83M effective ops/s under direct write-heavy contention while shedding 2.36–3.07% of `Add` attempts (§9.1, §9.4, §9.7).

---

## 1. Background: The Hardware Model

To reason about concurrent cache design, we first establish a precise model of the hardware execution environment.

### 1.1 The CPU Cache Hierarchy

Modern multi-core processors organize memory into private L1 caches (typically 32–64 KiB, ~4–5 cycles) backed by larger L2/L3 tiers and main memory (~200–300 cycles), so an L1 miss is over an order of magnitude more expensive than an L1 hit. Crucially, L2/L3 sharing varies across targets: conventional x86_64 servers typically attach private L2 caches (256 KiB–1 MiB) per core beneath a shared multi-megabyte L3 cache, whereas our Apple M2 evaluation CPU organizes its 8 cores into a 4-core Performance cluster (shared cluster L2) and a 4-core Efficiency cluster (smaller shared cluster L2) backed by a System Level Cache (SLC).

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

We separate the resulting design into three orthogonal guarantees—**Safety** (no torn payloads, invalid pointer exposure, or concurrent unsynchronized mutation), **Progress** (lock-free bounded-probe reads and retry-bounded, best-effort writes), and **Performance** (reduced coherence traffic and predictable tail latency)—and contribute:

* **Contention-Bounded Set-Associative Mechanism:** A 64-way set associative architecture utilizing chunked bitmasks and hardware `CTZ`, eliminating global clock hands and open-addressed tombstone maintenance.
* **Hybrid Memory Architecture:** Off-heap `mmap` allocation for pointer-free lock-free synchronization state (bitmasks, signatures, seqlocks) combined with on-heap `atomic.Pointer` SoA arrays to immutable values so the Go GC safely traces live strings, closures, and parameter slices.
* **Padded Seqlocks:** A cache-line-aligned structural layout using monotonic 64-bit seqlocks that yields lock-free bounded-probe reads and eliminates false sharing across per-slot `slotState` sequence counters (and per-stripe `statStripe` stats).
* **Load-Shedding Best-Effort Admission Protocol:** A contention-bounded, non-linearizable write protocol that caps per-claim ownership CAS retries to $R=10$ attempts (with post-claim signature-word CAS updates outside $R$); under sustained contention, admission or redundant updates may be dropped rather than spinning unboundedly.

The rest of this paper is structured as follows: Section 2 details the hybrid memory architecture, Section 3 discusses related work, Section 4 outlines the set-associative design, and the remaining sections analyze the eviction algorithm, safety, and performance.

## 2. Hybrid Memory Architecture and GC Isolation

**Scanner Heap Pressure.** Objects that escape to the heap are scanned during the mark phase. A cache holding thousands of string values generates a large root set for the GC to scan, increasing mark phase duration proportionally.

`liteLRU` addresses this through two non-trivial architectural changes:

1. **64-Way Set Associativity over Hash Maps**: The prior open-addressed designs considered by `liteLRU` use tombstones or comparable reclamation mechanisms to preserve concurrent probe sequences. Their maintenance and reclamation costs complicate predictable deletion behavior under sustained eviction. `liteLRU` instead overwrites a victim in place within a fixed set. An incoming route hash is mathematically mapped to a specific 64-slot bucket (a "set").
2. **SWAR Signature Scanning**: To filter candidate slots within a 64-slot set before dereferencing heap pointers, `liteLRU` maintains a `uint64` signature word per 8 slots. Each byte in the word stores an 8-bit hash signature. SWAR-style scalar bitwise operations test eight 8-bit signatures per machine word, reducing pointer dereferences and branchy per-slot comparisons before full-key verification. Lookups are bounded by fixed 64-way associativity. An 8-bit signature carries a 1/256 per-slot false-positive probability (about $1 - (1 - 1/256)^8 \approx 3\%$ across an 8-slot word). Any SWAR match is confirmed by a full string comparison before the entry is returned; false positives incur extra full-key comparisons across colliding slots, never an incorrect response. Layout-wise, `sigs` is a contiguous 64-byte `[8]atomic.Uint64` array placed at byte offset 32 inside every 128-byte `chunk` (`valid`, `accessed`, `writing`, and an 8-byte pad precede it). Because `chunks` is page-aligned via `mmap` and `sizeof(chunk) == 128`, `sigs` occupies bytes `[32, 96)` of a single 128-byte coherence line on Apple Silicon (one fetch) and `[32, 64)` plus `[64, 96)` on 64-byte x86_64 lines (exactly two fetches).

To achieve this without sacrificing safety, `liteLRU` employs a **hybrid memory architecture**. High-frequency concurrency primitives (bitmasks, seqlocks) are allocated purely off-heap via anonymous `mmap` to reduce GC allocation objects and eliminate pointer write barriers on metadata mutations (§9.12). Conversely, data arrays containing Go pointers (keys, values, and handler functions) remain on the standard Go heap behind `atomic.Pointer`. This separation ensures the GC safely tracks and collects dynamic strings or closures, avoiding dangling-pointer hazards while keeping high-churn metadata invisible to the GC.

---

## 3. Related Work

The problem of concurrent cache design generally branches into two orthogonal domains: optimizing the eviction *policy* for higher hit rates, and optimizing the concurrent *mechanism* for lower CPU overhead. `liteLRU` is strictly a contribution to the latter.

**Concurrent CLOCK Mechanisms.** The closest published system to `liteLRU` is MemC3 [[16]](#ref-memc3), which optimized Memcached by introducing a concurrent CLOCK with 1-bit recency tags and optimistic locking to remove mutexes from the read path. `liteLRU` builds on this foundation by applying it to the Go runtime (off-heap `mmap`, zero-allocation SoA), but replaces MemC3's sequential bit-checks with bulk 64-bit bitmask evaluations via hardware `CTZ` intrinsics, drastically reducing eviction scan latency.

**Advanced Eviction Policies.** Recent systems like CAR, CLOCK-Pro [[13]](#ref-clockpro), S3-FIFO [[5]](#ref-s3fifo), and SIEVE [[6]](#ref-sieve) demonstrate that scan-resistant or frequency-aware eviction policies outperform 1-bit CLOCK [[3]](#ref-clock) on web workloads. These policy innovations are largely orthogonal to the mechanism: a SIEVE or S3-FIFO algorithm still requires a concurrent implementation strategy to scale across cores without locking.

**Production Go Caches.** Existing Go caches like `freecache`, `bigcache`, and `ristretto` achieve high throughput via sharding and background eviction goroutines. `liteLRU` takes a different approach: rather than amortizing eviction cost over background threads or coarse-grained shard locks, `liteLRU` eliminates the locking overhead on lookups via bitmask mathematics and seqlocks, enabling lock-free bounded-probe reads with retry-bounded, best-effort writes.

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

The Structure of Arrays (SoA) alternative, widely employed in high-performance and SIMD-oriented computing [[7]](#ref-soa), separates fields into parallel arrays (illustrated conceptually below, and realized in `liteLRU` via Go `atomic.Pointer` arrays to immutable heap objects):

```go
// Conceptual SoA layout (16-byte string headers, 8-byte function/pointer fields)
methods  []string    // contiguous conceptual layout
paths    []string
handlers []uintptr

// Actual hardened liteLRU Go representation: 8-byte single-word atomic.Pointer slices
methods  []atomic.Pointer[string]
paths    []atomic.Pointer[string]
handlers []atomic.Pointer[HandlerFunc]
params   []atomic.Pointer[[]Param]
```

In `liteLRU`'s actual implementation, each SoA slot element is a single 8-byte `atomic.Pointer`, packing 8 pointers per 64-byte cache line (16 per 128-byte line) and avoiding struct padding waste between heterogeneous `string`/`uintptr`/`[]Param` fields when scanning candidate indices. Note that reading a string key still requires dereferencing the immutable `*string` pointer after a SWAR signature match, which is why SWAR pre-filtering is critical when chunks are heavily populated.

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

The foundational CLOCK algorithm [[3]](#ref-clock) approximates LRU by abandoning exact recency ordering (later extended by scan-resistant refinements such as CLOCK-Pro [[13]](#ref-clockpro)). Instead of tracking *which* entry is least recently used, 1-bit CLOCK tracks *whether* each entry has been accessed since the last eviction sweep — a single bit per entry.

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

The bitmask representation drastically reduces the cost of scanning. A core seeking eviction reads $V_k$, $A_k$, and $W_k$. If $C_k = 0$, either all valid slots have been accessed or free candidates are currently locked in $W_k$. The core atomically clears valid access bits via `chk.accessed.And(^validBits)`. We explicitly distinguish **CLOCK recency imprecision** from **ownership safety**: stale $A_k$ observations and bulk clearing affect only approximate victim preference (bits set by concurrent readers microseconds earlier may be cleared alongside stale bits), whereas exclusive slot ownership is enforced strictly by the $W_k[i]$ CAS (`0 -> 1`) before $V_k[i]$ or slot contents are touched.

**Writer Progress and Claim Protocol.** 

The eviction search process executes as follows:
1. Extract candidate index $i = \text{CTZ}(C_k)$.
2. Execute an atomic Compare-And-Swap (CAS) to set $W_k[i] \leftarrow 1$.
3. If the CAS fails (another writer claimed it), restart at step 1 for the same chunk.
4. If $W_k[i] = 1$ is successfully claimed, proceed with the in-place 64-way set associative write protocol defined in §6.

To guarantee contention-bounded admission and bound the worst-case scenario where a chunk is pathologically saturated by concurrent writers (e.g., a Thundering Herd), `liteLRU` employs **Load-Shedding Eviction**. The `findVictim` loop attempts to claim a write-lock (`writing` bit) on an eviction candidate. If it fails to claim any slot after 10 retries, it returns a sentinel failure value (`0xFFFFFFFF`). The `Add` operation intercepts this sentinel and safely drops the cache admission. By sacrificing absolute admission under extreme contention, `liteLRU` bounds write-side work to a fixed number of CAS attempts; under sustained contention, admission may be dropped.

### 5.4 Eliminating the Global Clock Hand

Because `liteLRU` hashes each key to a fixed 64-slot set $k = h(r) \bmod N$, there is no global clock-hand atomic: each chunk maintains its own local `(V_k, A_k, W_k)` bitmasks as an independent 64-way set hand. Both lookup (`Get`) and eviction (`Add`/`findVictim`) operate **strictly within chunk $k$** and never probe or spill into neighboring chunks, eliminating cross-chunk sweep contention while trading off strict per-set capacity bounds (analyzed in §9.11). 

---

## 6. Algorithm and Correctness

### 6.1 State Definitions and Layout

The cache state is divided into global configuration, per-chunk metadata, and per-slot data:

* **Global:** Total capacity $C$ and number of chunks $N = C/64$.
* **Per-Chunk (off-heap `mmap`):** A `chunk` struct containing 64-bit masks $V_k$ (valid bits), $W_k$ (writing/ownership bits), $A_k$ (access/recency bits), and an array of 8 `uint64` SWAR signatures for fast scanning.
* **Per-Slot:** A `slotState` (off-heap `mmap`, padded to the target platform's $L$-byte coherence stride) containing the monotonic 64-bit seqlock $S_i$, alongside Structure of Arrays (SoA) slices on the Go heap (`[]atomic.Pointer[string]`, `[]atomic.Pointer[HandlerFunc]`, `[]atomic.Pointer[[]Param]`) pointing to immutable key and payload objects.

### 6.2 API Semantics, State Machine, and Control Flow

**Best-Effort, Non-Linearizable Cache Semantics and Duplicate Keys.** `Add(key, value)` is a **best-effort, non-linearizable set-associative cache insertion/update**, *not* a globally serialized or linearizable upsert:
1. If an existing valid entry for `key` is found during the initial chunk scan, `Add` acquires exclusive slot ownership via $W_k[i]$ and updates its value in place under the seqlock. If $W_k[i]$ is already owned by another writer for that exact slot, `Add` sheds the redundant concurrent update (`drops++`) and returns.
2. If no matching entry is observed, `Add` claims an eviction victim in chunk $k$ via `findVictim(k)`. Under simultaneous cold-start misses on the same `key`, two writers holding *distinct* $W_k$ bits can both insert into different slots (e.g., Writer A inserts value $v_A$ into slot 4 and Writer B completes later with $v_B$ into slot 12, leaving both slots valid).
3. Because `Get` scans slots $0\dots 63$ in deterministic ascending order and sets $A_k$ only for the first clean match, `Get` returns $v_A$ from slot 4 even though Writer B completed later (`Add` does not guarantee that later-completed admissions supersede earlier ones). Furthermore, future fast-path updates touch only the lowest-index match (slot 4), leaving slot 12 unmodified; if slot 4 is later evicted, the older duplicate in slot 12 can become visible again (**stale duplicate resurrection**) until reclaimed by CLOCK sweeps. Callers requiring strict per-key coherence over mutable state must serialize updates externally rather than relying on `Add` as a linearizable upsert.

**Slot Publication State Machine.** Each slot transitions through three logical states governed by $(W_k[i], V_k[i], S_i)$:
* **INVALID** ($V_k[i] = 0$): Slot is empty or cleared prior to mutation; readers skip on $V_k[i] = 0$.
* **WRITING** ($W_k[i] = 1, S_i \equiv 1 \pmod 2$): Exclusively owned by one writer. `Add` clears $V_k[i] \leftarrow 0$ (on eviction/insert) and advances $S_i$ from $2g$ to $2g+1$ before publishing immutable `atomic.Pointer` fields, updating the SWAR signature byte, and setting $A_k[i] \leftarrow 1$.
* **VALID** ($V_k[i] = 1, S_i \equiv 0 \pmod 2$): Writer closes generation $S_i \leftarrow 2g+2$, sets $V_k[i] \leftarrow 1$ (linearization point for reader visibility), and releases $W_k[i] \leftarrow 0$. `Clear()` atomically acquires all 64 bits of $W_k$ via `CompareAndSwap(0, ^uint64(0))` (never an unconditional store), clears $V_k/A_k/\text{sigs}$, and advances $S_i$ monotonically ($S_i + 1 \to S_i + 2$) rather than resetting to zero.

**Get(key)**
1. Hash the key to find chunk $k$, stripe index `hash & 63`, and 8-bit signature `sig8`.
2. Scan the 8 SWAR signature words (`i = 0..7`) using byte-wise comparison.
3. For each candidate slot $i$ in ascending slot order, check $V_k[i] = 1$.
4. Load $S_{i, \text{start}}$. If odd, skip slot (active write).
5. Perform full-key equality verification (`slotKeyMatches`) by loading the single-word `atomic.Pointer[string]` key fields (`methods[i]`, `paths[i]`) and comparing strings; if unequal (SWAR false positive or concurrent key change), skip slot.
6. Load the single-word value pointers (`handlers[i]`, `params[i]`) and copy any `[]Param` into caller buffer `dst`.
7. Load $S_{i, \text{end}}$. If $S_{i, \text{start}} \neq S_{i, \text{end}}$, skip slot (concurrent write).
8. Check $A_k[i]$ and only if $A_k[i] = 0$, atomically OR bit $i$ into $A_k$; increment hit counter and return.
9. If no slot matches cleanly, increment miss counter and return miss.

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

**Clear()**
1. Iterate sequentially over chunks $k = 0 \dots N-1$.
2. Loop loading $w = W_k$: if $w \neq 0$, yield via `runtime.Gosched()` and reload; once $w = 0$, execute atomic `CompareAndSwap(W_k, 0, ^uint64(0))`. If the CAS fails because a writer claimed a slot bit between the load and CAS, retry from the load (preventing dual ownership of any slot).
3. With $W_k = \text{all-ones}$ held, atomically clear metadata: execute `chk.valid.Store(0)`, `chk.accessed.Store(0)`, and `chk.sigs[j].Store(0)` for every signature word $j \in \{0 \dots 7\}$ (all atomic stores).
4. For each slot $i \in [0, 63]$ of chunk $k$, monotonically advance $S_i \leftarrow S_i + 1$ (odd), set `params[i]`, `methods[i]`, `paths[i]`, and `handlers[i]` to `nil`, and close generation $S_i \leftarrow S_i + 2$ (even).
5. Release chunk ownership (`chk.writing.Store(0)`), then advance to chunk $k+1$ (and zero the 64 `statStripe` counters once all chunks finish).

*Progress Note on `Clear()`*: While the `CompareAndSwap(0, ^uint64(0))` guarantees exclusive per-chunk safety, `Clear()` is **blocking and not starvation-free under continuous concurrent writes**: if writers continually acquire non-zero bits in $W_k$ across yields, `Clear()` may spin indefinitely waiting for a quiescent $W_k = 0$ window. Callers requiring deterministic `Clear()` completion under sustained write traffic must externally quiesce writers.

**Linearization Points and Stale Mask Snapshots.** Because `findVictim` loads $V_k$, $A_k$, and $W_k$ in separate atomic instructions before executing CAS on $W_k$, a reader may set $A_k[i] \leftarrow 1$ between loading $A_k$ and claiming $W_k[i]$. We distinguish **eviction-policy fidelity** from **ownership safety**: stale $A_k$ observations are explicitly permitted by approximate CLOCK and only affect victim preference, whereas exclusive slot ownership is linearized solely at the successful $W_k$ CAS. Once $W_k[i]=1$ is held, no other writer or `Clear()` can mutate slot $i$, $V_k[i]$, or $S_i$ until $W_k[i]$ is released.

### 6.3 Go Memory Model Safety

Seqlock validation alone does not make concurrent accesses to multi-word Go structs (like 16-byte `string` headers or 24-byte `[]Param` headers) race-free: loading `ptr` and `len` separately across a concurrent writer can tear and trigger out-of-bounds reads *before* $S_{i,\text{end}}$ is checked, and reusing a slice backing array in place creates a Go data race with concurrent readers.

`liteLRU` therefore combines off-heap synchronization metadata with **single-word `atomic.Pointer` publication of immutable heap values**:
1. Every concurrently read key/value field (`methods`, `paths`, `handlers`, `params`) is a single `atomic.Pointer` pointing to an immutable object (`string`, `HandlerFunc`, or freshly copied `[]Param`) that is never mutated after publication.
2. Writers to any slot $i$ are strictly serialized by $W_k[i]$ (`Add` fast-path updates, `Add` evictions, and `Clear` all acquire $W_k[i]=1$ before touching $V_k[i]$, $S_i$, or payload pointers).
3. The sequence counter $S_i$ is a monotonic `atomic.Uint64` (never reset on `Clear`) padded to $L$ bytes and never copied after initialization.
4. All synchronization primitives rely on Go's documented sequentially consistent `sync/atomic` total order [[15]](#ref-gomem).

### 6.4 Correctness Theorems

**Theorem 1 (Bounded-Probe Lock-Free Reads).** *Any thread executing `Get` terminates in a bounded number of metadata probes (at most 8 SWAR words and 64 slots; per-candidate key comparison cost is $O(|\text{key}|)$), regardless of the state or execution speed of concurrent writers.*

*Proof:* `Get` contains no unbounded loops and never retries on seqlock conflict. It evaluates at most 8 SWAR words and 64 slot indices; for each matching signature byte it executes bounded atomic loads ($V_k$, $S_{i,\text{start}}$, single-word `atomic.Pointer` key/value pointers, $S_{i,\text{end}}$) and at most one full-key comparison before either advancing to the next candidate or returning a hit (with one optional atomic `Or` on $A_k$). $\square$

**Theorem 2 (Bounded Ownership-Claim Retries).** *Any `Add` execution decomposes as $T_{\mathrm{Add}} = T_{\mathrm{lookup}} + T_{\mathrm{claim}} + T_{\mathrm{publication}}$, where ownership-claim CAS retries on $W_k$ are capped at $N_{\mathrm{claim,retries}} \le R = 10$ per matched fast-path slot and at $N_{\mathrm{claim,retries}} \le R = 10$ inside `findVictim` (so an operation encountering $d \ge 0$ matching duplicate slots performs at most $R$ claim retries per matched slot plus at most $R$ retries in `findVictim`), whereas post-claim signature-word publication ($T_{\mathrm{publication}}$) is not bounded by $R$. Unlike `Get` and `Add`, `Clear()` is blocking and requires quiescent or bounded writer overlap on $W_k$ to terminate.*

*Proof:* In `findVictim`, every failed `W_k` CAS and every `A_k`-clearing pass (`chk.accessed.And(^validBits)`) increments `retries`, returning `0xFFFFFFFF` once `retries >= 10` ($N_{\mathrm{claim,retries}} \le R = 10$). In the fast-path chunk scan, each matched duplicate candidate slot $i$ enters a loop retrying `W_k` CAS at most $R=10$ times on unrelated-slot changes (dropping immediately if $W_k[i]=1$ or on retry exhaustion, and continuing the scan only if the matched slot was invalidated before `W_k[i]` was acquired). Thus Theorem 2 bounds **per-claim ownership-acquisition retries**, not total wall-clock latency or total operation CAS count: OS scheduling, Go heap allocations for immutable wrappers/slices, scanning $d$ distinct matching duplicate slots, and post-claim `chk.sigs[sigWordIdx].CompareAndSwap` updates (which retry until winning against concurrent writers touching *other* slots in the same 8-byte signature word) lie outside $R$. Conversely, `Clear()` retries its `w == 0` check and `CAS(W_k, 0, ^uint64(0))` indefinitely until acquiring the chunk. $\square$

**Theorem 3 (Safety Invariant).** *Assuming fewer than $2^{63}$ complete write generations occur to a single slot during one active `Get` probe (precluding 64-bit counter wraparound between $S_{i,\text{start}}$ and $S_{i,\text{end}}$), no two writers may simultaneously mutate the same slot, and any accepted read in `Get` observes a complete, untorn key/value snapshot from a single publication generation.*

*Proof:* Mutual exclusion among writers (`Add` fast path, `Add` eviction, and `Clear`) is enforced by $W_k[i]$: every writer acquires $W_k[i]: 0 \to 1$ via CAS (or atomically claims all 64 bits via `CAS(W_k, 0, ^uint64(0))` in `Clear`) before modifying $V_k[i]$, $S_i$, or slot pointers, and holds $W_k[i]=1$ until the write completes. Under Go's sequentially consistent atomic order, writer generation $g$ stores all immutable `atomic.Pointer` fields while $S_i = 2g+1$ (odd) and only then stores $S_i \leftarrow 2g+2$ (even) before setting $V_k[i] \leftarrow 1$. Under the bounded-generation hypothesis ($< 2^{63}$ complete writes to slot $i$ during a single probe), observing $S_{i,\text{start}} = S_{i,\text{end}} = 2g+2$ (even) implies that all `slotKeyMatches` and value pointer loads occurred strictly after writer $g$'s $S_i \leftarrow 2g+2$ store and strictly before writer $(g+1)$'s $S_i \leftarrow 2g+3$ store. Because each published `*string`, `*HandlerFunc`, and `*[]Param` target is immutable after publication, the reader never observes a torn header or concurrent backing-array mutation even if a concurrent writer begins generation $g+1$ before $S_{i,\text{end}}$ is checked. $\square$

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

Because the `mmap` allocation guarantees $\text{addr}(S_0) \pmod L = 0$ (page alignment implies $L$ alignment for $L \le 4096$), and $\text{addr}(S_{i+1}) = \text{addr}(S_i) + L$, adjacent `slotState` entries reside on disjoint, non-overlapping $L$-byte aligned cache lines. A write to $S_i$ can require coherence activity for its own line but does not require ownership transfer for a distinct `slotState` line. $\square$

We explicitly clarify the **scope of padding isolation**: $L$-byte padding isolates distinct `slotState` sequence counters and distinct `statStripe` stats structs onto separate coherence lines. By contrast, all 64 slots belonging to the same chunk $k$ intentionally share the chunk's compact `uint64` bitmasks ($V_k$, $A_k$, $W_k$) and 8-word `sigs` array. Packing 64 slots into one `uint64` reduces metadata word count by 64× compared to per-slot flags, trading intrachunk mask sharing for constant-width `CTZ` victim selection. `liteLRU` mitigates this intrachunk sharing in two ways: `Get` reads `chk.accessed` first and only issues an atomic `Or` when the bit is unset, and `Add` bounds retry loops on shared chunk words to $R=10$. Similarly, sharded counters use 64 padded `statStripe` structs indexed by `hash & 63`, distributing rather than eliminating coherence contention across stripes.

---

## 8. Benchmark Methodology and Artifacts

Standard Go micro-benchmarks use `b.RunParallel`, which distributes `b.N` iterations across goroutines via `pb.Next()`. While `b.RunParallel` amortizes synchronization over per-goroutine iteration batches rather than executing a shared atomic fetch-and-add on every single call, it still adds harness overhead and aggregates only mean `ns/op` without latency percentiles. Running `BenchmarkParallelLRUCache` (`-count=5`) on the hardened race-free codebase (`80% Get / 20% Add`, 1024 capacity) yields medians of **80.99 ns/op** (1 core), **53.66 ns/op** (2 cores), **32.49 ns/op** (4 cores), and **35.15 ns/op** (8 cores, range `32.55–49.55 ns/op`, reflecting heterogeneous P/E-core scheduling across Apple M2's 4 performance + 4 efficiency cores) with **0 allocs/op**.

Our custom evaluation suite (`benchmarks/hardened_eval/main.go`, 10 independent runs reporting median and interquartile range [IQR]) complements `b.RunParallel` by running tight worker loops over pre-allocated key arrays with no shared iteration counter and recording sampled or standalone latency percentiles directly.

---

## 9. Evaluation

**Platform:** Apple M2 (8-core ARM64: 4 performance + 4 efficiency cores, 24 GB RAM, macOS 14.5, Go 1.25.7). Cache capacity 1024 entries unless stated otherwise. All core microbenchmark sections (§§9.1–9.4, §9.6–9.7, §9.11–9.12) reflect **10-run medians (with IQR where noted)** on the hardened race-free codebase (`atomic.Pointer` SoA + $W_k$ serialization); §§9.8–9.10 preserve the earlier pre-hardening `vegeta` HTTP snapshot (`f826423`).

### 9.1 Baseline Throughput and Tail Latency

**Workload:** 8 concurrent workers, $1.6 \times 10^6$ total operations, 80% `Get` / 20% `Add`, paths drawn independently at random from a 1000-route uniform-pool mix (70% over hot 200 keys, 30% over 1000 keys; `benchmarks/hardened_eval`). Measured over 10 runs on the hardened implementation against `otter` (`v1.2.4`) and a lock-protected doubly-linked-list `Mutex LRU`:

| Cache Implementation | Median Ops/sec | IQR (Q1–Q3) | Median p50 | Median p99 |
|----------------------|---------------:|-------------:|-----------:|-----------:|
| `liteLRU`            | 21,596,758     | 20.89M–22.70M | 333 ns    | 1.69 µs    |
| `otter` (`v1.2.4`)   | 15,694,884     | 15.12M–15.89M | 292 ns    | 51.06 µs   |
| `Mutex LRU`          | 7,154,696      | 7.10M–7.26M  | 125 ns     | 28.79 µs   |

*Note: Latency percentiles in this table were sampled at a 1% rate to limit observer effect on RFO traffic while preserving concurrent throughput.*

Across 10 runs, hardened `liteLRU` delivers a **3.02×** median throughput advantage over `Mutex LRU` and **1.38×** over `otter` (`v1.2.4`), while keeping sampled p99 latency at 1.69 µs (vs. 51.06 µs for `otter` and 28.79 µs for `Mutex LRU`).

### 9.2 Throughput Scaling Under Contention

To measure multi-core scaling under high eviction and cache-miss churn on the hardened implementation, we scale a 1.6M operation **Zipfian** workload ($s=1.001$, $N=100{,}000$, capacity 1024, 80% `Get` / 20% `Add`) across 1 to 8 workers (10 runs per core count):

| Cores | Median Ops/sec | IQR (Q1–Q3)   | Scaling Factor |
|------:|---------------:|--------------:|---------------:|
| 1     | 10,874,389     | 10.61M–11.24M | 1.00×          |
| 2     | 18,176,343     | 16.98M–18.39M | 1.67×          |
| 4     | 21,363,718     | 19.53M–22.20M | 1.96×          |
| 8     | 23,338,813     | 22.78M–24.15M | 2.15×          |

Scaling rises sharply to 2 cores (1.67×) and tapers between 4 and 8 cores (1.96× to 2.15×). Both hardware asymmetry (Apple M2 pairs 4 high-frequency performance cores with 4 slower efficiency cores when concurrency exceeds 4) and hot-chunk atomic coherence traffic ($V_k, A_k, W_k$, and hot seqlock writes under heavy Zipf skew) contribute to this sub-linear curve.

### 9.3 Latency Percentiles (ARM64)

Measured over 10 uncached executions of `TestConcurrentLatency` (8 workers, 1.6M operations, `17.87M ops/s` median throughput):

| Percentile | Raw 10-Run Median | Raw Observed Range |
|:----------:|------------------:|-------------------:|
| p50        | 167 ns            | 167–250 ns         |
| p99        | 1,104 ns          | 1.00–1.17 µs       |
| p99.9      | 3.21 µs           | 1.75–10.38 µs      |
| Max        | 5.97 ms           | 4.34–9.74 ms       |

*Measurement note:* These raw latencies include per-sample `time.Now()` timing overhead (~40 ns/call on ARM64); under a simplified additive constant subtraction, underlying operation medians correspond approximately to ~127 ns (p50), ~1.06 µs (p99), and ~3.17 µs (p99.9). The extreme tail (`Max` 4.34–9.74 ms) reflects Go goroutine/OS scheduling preemption on one or more of the eight workers; the test does not isolate scheduler preemption from runtime background activity.

### 9.4 Hardened Ablation Study

To isolate the contribution of SWAR signatures, $L$-byte padding, and lock-free 64-way set associativity **strictly without allocation-placement confounding**, all three set-associative variants (`Full hardened`, `No SWAR (mmap)`, and `No Pad (mmap)`) allocate their `states` and `chunks` metadata via off-heap `mmap` and share identical 4-slice `atomic.Pointer` SoA fields (`benchmarks/hardened_eval`, 10 runs, no timing sampling, capacity 1024, 8 workers):
- **Regime A0 (Zero-Overflow Pre-Warmed 100% `Get`, 960 keys):** Constructed with `<= 60` keys per chunk across all 16 sets and 100% pre-warmed prior to timing (`0` misses, `0` evictions, `0` drops).
- **Regime A1 (Uniform-Pool 80/20, 1,000 keys):** Empirically audited hash distribution across the 16 chunks shows `minH=56`, `maxH=70`, with **6/16 sets overflowing** ($H_k > 64$, 30 excess keys total), yielding `1,262,786` hits, `17,214` misses (`98.66%` hit rate), and `353` drops—demonstrating that $N_{\text{keys}} = 1{,}000 < 1{,}024$ is *low-eviction* rather than eviction-free.
- **Regime B (Zipfian 80/20, $N=100{,}000$, $s=1.001$):** Continuous misses and active CLOCK eviction.
- **Regime C (Zipfian 50/50, $N=100{,}000$, $s=1.001$):** Active eviction paired with heavy write contention.

We also compare against `True MutexLRU` (`sync.Mutex` + doubly-linked list) and `Map+RWMutex*` (Go `map` + `sync.RWMutex` omitting per-read LRU promotion):

| Variant | Regime A0: Zero-Overflow Resident 100% Get | Regime A1: Uniform 80/20 (6/16 sets overflow) | Regime B: Zipf 80/20 Active Evict | Regime C: Zipf 50/50 Write-Heavy |
|---------|-------------------------------------------:|----------------------------------------------:|----------------------------------:|---------------------------------:|
| **Full Hardened `liteLRU`** | 27.85M (IQR 26.98M–29.88M) | 22.50M (IQR 21.84M–23.01M) | **27.97M** (IQR 26.97M–29.84M) | **22.28M** (IQR 21.38M–23.18M) |
| No SWAR (`mmap`) | 18.85M (**-32.3%**) | 22.94M (+2.0%) | 12.68M (**-54.7%**) | 11.38M (**-48.9%**) |
| No Padding (`mmap`) | **29.20M** (**+4.8%**) | 22.64M (+0.6%) | 26.41M (**-5.6%**) | 22.03M (-1.1%) |
| True `MutexLRU` | 8.43M (**-69.7%**) | 7.18M (**-68.1%**) | 6.45M (**-76.9%**) | 5.53M (**-75.2%**) |
| `Map+RWMutex*` (non-LRU) | 11.76M (**-57.8%**) | **24.04M** (+6.9%) | 13.57M (**-51.5%**) | 8.68M (**-61.0%**) |

Controlling both allocation placement (`mmap` vs. `mmap`) and Regime A set occupancy reveals four cleaner insights:
1. **SWAR matters both on populated sets and on misses:** Even with **zero** misses and evictions in `Regime A0` (960 resident keys, 60/64 slots populated per chunk), removing SWAR drops throughput by **-32.3%** (`18.85M` vs. `27.85M`), because linear probing must inspect `atomic.Pointer` string headers across ~60 populated slots rather than filtering with 8 `uint64` signature words. Under heavy miss/eviction churn (`Regimes B & C`), removing SWAR cuts throughput by **-54.7%** and **-48.9%**. (In `Regime A1`, where 70% of traffic concentrates on 200 keys placed in low-index slots, sequential scans find hot keys early enough that SWAR adds +2.0% overhead.)
2. **Isolating padding stride cleanly reduces the density-vs-contention tradeoff:** When both variants sit in off-heap `mmap`, removing 128-byte padding yields a clean **+4.8%** gain in read-only L1-dense `Regime A0` (`8 KiB` vs. `128 KiB` seqlock footprint), breaks even in `Regime A1` (`+0.6%`), and degrades active eviction throughput by **-5.6%** in `Regime B` (and `-1.1%` in `Regime C`) rather than the +16.6% heap/aliasing artifact seen prior to fixing placement.
3. **Set-associative lock freedom avoids mutex collapse across all regimes:** Even without per-read LRU promotion, `Map+RWMutex*` loses **-57.8%** to **-61.0%** relative to `liteLRU` under uniform 960-key contention and Zipf churn, while `True MutexLRU` drops **-68.1%** to **-76.9%**.

### 9.5 x86_64 Smoke Test

To verify behavioral consistency across architectures, we evaluated `liteLRU` on an x86_64 Docker container instance. The x86_64 measurement is a behavioral smoke test only; its virtualized environment and different microarchitecture do not support a direct cross-platform performance comparison.

| Architecture | Cores | Ops/sec (x86_64) |
|--------------|------:|-----------------:|
| x86\_64       | 8     | 13,155,469       |

*Note: The x86_64 evaluation was executed within a virtualized Docker Desktop environment. Because hypervisor layers introduce non-deterministic scheduling jitter and distort cache-coherence traffic (such as Read-For-Ownership loops), these metrics are presented strictly as a functional and behavioral smoke test to verify cross-platform correctness of the atomic bitmask and TZCNT mechanics, rather than a bare-metal microarchitectural performance baseline.*

### 9.6 Zipfian Hit-Rate Evaluation (Matched Capacities)

Because `NewLRUCache` rounds requested capacities up to the next power of two (`nextPowerOfTwo`), evaluating arbitrary non-power-of-two sizes (`25k, 50k, 75k, 95k`) against an exact-capacity cache gives `liteLRU` 31%–75% more effective slots (`32,768`, `65,536`, `131,072`). To eliminate that capacity confounding, we compare both caches at **matched power-of-two capacities** (`16,384`, `32,768`, `65,536`, corresponding to 16.4%, 32.8%, and 65.5% of the $N=100{,}000$ working set) with a 20% warmup phase over 10 independent runs (`benchmarks/zipf_bench.go`, `zipf_low_skew_bench.go`):

| Matched Capacity | `liteLRU` ($s=1.001$) | `otter` ($s=1.001$) | `liteLRU` ($s=0.800$) | `otter` ($s=0.800$) |
|------------------|----------------------:|--------------------:|----------------------:|--------------------:|
| 16,384 (16.4%)   | 78.97%                | **80.62%**          | 55.25%                | **62.86%**          |
| 32,768 (32.8%)   | 86.57%                | **87.37%**          | 69.58%                | **72.04%**          |
| 65,536 (65.5%)   | 94.38%                | **94.70%**          | **87.19%**            | 87.02%              |

At matched capacities, Otter's [[18]](#ref-otter) S3-FIFO policy consistently outperforms 1-bit chunked CLOCK at tighter cache ratios (especially under flatter $s=0.800$ skew at 16.4% capacity: 62.86% vs. 55.25%, a +7.61 pp advantage), where frequency filtering avoids churning one-hit tail keys. At high skew ($s=1.001$), `liteLRU` stays within **0.32–1.65 pp** of Otter, and matches Otter once capacity reaches 65.5% under $s=0.800$ (87.19% vs. 87.02%). This confirms that `liteLRU`'s contribution is mechanism throughput and predictability rather than beating S3-FIFO on eviction quality.

### 9.7 Write-Heavy Workloads and Semantic-Equivalence Controls

We omit Ristretto from this benchmark because, as documented in [[18]](#ref-otter), it exhibits hit-rate degradation under extreme concurrent write pressure. Comparing write-heavy caches requires separating two distinct workloads (`benchmarks/hardened_eval`, 10 runs, 8 workers, capacity 1024, Zipfian $s=1.001$):
1. **Direct overwrite contention (`9.7a`, $N=1{,}000$, 50/50 and 20/80 `Get`/`Add`):** Threads issue explicit `Add`/`Set` operations directly against hot keys. Under `liteLRU`, 2.36%–3.07% of redundant same-slot overwrites are shed via `W_k` collision drops (non-linearizable best-effort updates), whereas `otter` queues every write internally; discounting dropped writes yields **effective** completed throughput, though semantic work differs when strict update publication is required.
2. **Immutable insert-on-miss maintenance (`9.7b`, $N=1{,}000$ and $N=5{,}000$):** Workers call `Get` and issue `Add`/`Set` **only on a cache miss**, representing memoized routing or immutable-value caches where both caches do equivalent working-set work (`0.00%` miss drops for `liteLRU`).

| Workload | `liteLRU` Attempted / Eff. (IQR) | Drops / Hit Rate | `otter` (`v1.2.4`) (IQR) / Hit | Throughput Ratio |
|---|---:|---:|---:|---:|
| `9.7a` Direct Overwrite 50/50 ($N=1\text{k}$) | 22.09M / **21.83M** (20.62M–22.41M) | 2.36% Adds / 99.53% | 6.10M (5.94M–6.24M) | **3.58×** eff. |
| `9.7a` Direct Overwrite 20/80 ($N=1\text{k}$) | 19.68M / **19.20M** (18.11M–19.68M) | 3.07% Adds / 99.47% | 3.93M (3.85M–3.98M) | **4.89×** eff. |
| `9.7b` Insert-on-Miss ($N=1\text{k}$, low miss) | **33.64M** (21.29M–34.61M) | 0.00% miss / 99.94% | **94.27M** (71.12M–109.22M) / 99.94% | **0.36×** (`otter` leads) |
| `9.7b` Insert-on-Miss ($N=5\text{k}$, evicting) | **15.63M** (14.90M–15.86M) | 0.00% miss / 75.34% | 13.36M (12.59M–13.77M) / **85.60%** | **1.17×** (`liteLRU` leads) |

These semantic-equivalence controls clarify the competitive boundary cleanly:
- When a cache holds immutable entries with a **99.94% hit rate** (`9.7b`, $N=1\text{k}$), `otter`'s read-stripe buffering achieves **94.27M ops/s** vs. `liteLRU`'s **33.64M ops/s** (`0.36×`), because read-mostly lookups avoid `liteLRU`'s 8-word chunk SWAR probe and shared chunk bitmask cache lines.
- Once capacity-driven misses and evictions rise (`9.7b`, $N=5\text{k}$), `liteLRU` retakes the throughput lead (**15.63M** vs. **13.36M ops/s**, **1.17×**) with `0.00%` dropped misses, while `otter`'s S3-FIFO retains a higher hit rate (**85.60%** vs. **75.34%**).
- Under heavy concurrent overwrite churn (`9.7a`), `liteLRU` sustains **19.20M–21.83M effective ops/sec** (**3.58×–4.89×**) specifically by converting 2.36%–3.07% of colliding hot-slot overwrites into load-shed drops rather than queuing them.

---


### 9.8 HTTP Router Integration (Pre-Hardening Snapshot `f826423`)

> **Historical Scope Note (§§9.8–9.10):** The end-to-end `vegeta` HTTP server benchmarks below (§§9.8–9.10) were captured prior to the race-hardening changes (`f826423`) without CPU/allocation/GC profiling and are preserved for historical application context only. Moreover, `benchmarks/http_server/server.go` configured `capacity = 75,000` (75% of the 100K key space); because `NewLRUCache` rounds non-power-of-two capacities up via `nextPowerOfTwo`, `liteLRU` provisioned 131,072 slots compared to `otter v1.2.4`'s exact 75,000 slots in these runs.

To observe behavior inside a standard Go `net/http` REST server bypassing JSON marshaling, we generated 10 seconds of concurrent load (64 workers) using `vegeta`, driving HTTP `GET` requests following a Zipfian distribution ($s=1.001$, $N=100,000$). The server simulates a backend endpoint by dynamically selecting one of 20 complex, nested JSON payloads per request:
1. **No Cache (Origin Only)**: The handler performs a full `json.Marshal()` on the complex payload structure for every single request before responding.
2. **Otter (`v1.2.4`)**: The handler caches the serialized JSON string. On a cache hit, it instantly writes the string, bypassing JSON marshaling completely.
3. **liteLRU**: The handler stores the serialized JSON string in the `liteLRU` parameter block. On a hit, it instantly writes the parameter string, bypassing JSON marshaling completely.

| Configuration | Throughput (Req/sec) | p50 Latency | p99 Latency | Max Latency |
|---------------|----------------------|-------------|-------------|-------------|
| No Cache      | 86,453               | 303 µs      | 1.36 ms     | 35.1 ms     |
| `otter` (`v1.2.4`) | 86,056          | 279 µs      | 1.41 ms     | 173.1 ms    |
| `liteLRU`     | **93,603**           | **276 µs**  | **1.20 ms** | **43.1 ms** |

**Observations (Single 10-second run):** `liteLRU` yields 93,603 req/sec vs. 86,453 req/sec (+8% because TCP/HTTP parsing dominates the per-request budget relative to `json.Marshal`), lowest p99 (1.20 ms), and 43.1 ms max latency. `otter` recorded slightly lower throughput than no-cache (86,056 req/sec) and a 173.1 ms maximum latency spike; isolating whether that tail spike came from asynchronous ingestion draining, lock contention, or Go runtime GC/scheduler effects would require profiling and repeated runs.

---

### 9.9 Reverse Proxy Integration (Pre-Hardening Snapshot `f826423`)

To observe caching when hits bypass simulated upstream latency, we tested a simulated reverse proxy endpoint adding a 50 ms base delay plus up to 20 ms random jitter on misses (`vegeta`, 10s, 64 workers). On hits, upstream delay and JSON serialization are skipped:

| Cache Implementation | Rate (Req/s) | p50 Latency | p99 Latency | Max Latency |
|----------------------|--------------|-------------|-------------|-------------|
| **liteLRU**          | **3,140 req/s** | **147 µs**  | 69.22 ms    | 74.14 ms    |
| Otter                | 3,139 req/s  | 152 µs      | 69.22 ms    | 72.45 ms    |
| Origin (No Cache)    | 1,101 req/s  | 57.36 ms    | 69.52 ms    | 90.55 ms    |

By bypassing the simulated upstream, both caches drop p50 latency from 57.36 ms to ~150 µs while p99 (69.22 ms) reflects cold-start upstream misses.

### 9.10 Dynamic Router Integration (Pre-Hardening Snapshot `f826423`)

Using the same pre-hardening `server.go` setup (`capacity = 75,000` rounded to `131,072` for `liteLRU`), we evaluated simulated regex routing (`/api/user/{id}/profile`) caching `HandlerFunc` and extracted `[]Param` retrieved into a caller stack buffer (`var pbuf [4]liteLRU.Param`):

| Cache Implementation | Rate (Req/s) | p50 Latency | p99 Latency | Max Latency |
|----------------------|--------------|-------------|-------------|-------------|
| **liteLRU**          | **95,708 req/s** | **253 µs**  | **1.39 ms** | **18.96 ms**|
| Otter                | 91,959 req/s | 269 µs      | 1.73 ms     | 37.92 ms    |
| Origin (No Cache)    | 78,680 req/s | 381 µs      | 1.93 ms     | 198.69 ms   |

Re-evaluating §§9.8–9.10 on the hardened codebase with matched power-of-two capacities, multi-run confidence intervals, and CPU/allocation profiling remains future work.

### 9.11 Controlled Occupancy, Set Pressure, and Adversarial Stress Tests

To characterize `liteLRU`'s set-associative and 1-bit CLOCK failure modes quantitatively (`benchmarks/adversarial_bench.go`, 1024-entry cache, 8 workers), we define per-chunk **set pressure** $P_k = H_k / 64$, where $H_k$ is the active working-set size hashing to chunk $k$:

| Experiment | Configuration | Observed Result |
|---|---|---|
| Single-set contention | 128 keys $\to$ chunk 0 (80/20 Get/Add) | 8,899,628 attempted / 8,871,516 effective ops/s; 1.58% Adds dropped; 32.56% hit rate |
| Associativity boundary | 64 vs. 65 keys in chunk 0 (1000 cyclic passes) | **100.00%** hit rate at $H_0=64$ ($P_0=1.00$) vs. **0.00%** at $H_0=65$ ($P_0=1.016$) |
| Occupancy (100% Get) | 1% / 50% / 100% fill (pure lookup) | 94.81M / 13.33M / 17.29M ops/s |
| Occupancy (80/20 Mixed) | 1% / 50% / 100% fill (with writes) | 67.56M / 18.15M / 14.30M ops/s (99.31% / 99.90% / 96.75% hit rate) |
| Sequential scan > capacity | 256 hot + 2048 scan keys | **0/256 (0.00%)** hot keys retained (CLOCK is not scan-resistant) |
| Single-key concurrent upsert | 8 workers, 1.6M `Add`s to `/single-hot` | 17.42M attempted / 6.50M effective ops/s; **62.68%** redundant writes shed |
| Key length scaling | 100% `Get`, 16 / 64 / 256 / 1024-byte keys | 64.03M / 36.05M / 12.63M / 3.77M ops/s |

These experiments clarify three structural behaviors:
1. **Set Pressure Regimes ($P_k \le 1$ vs. $P_k > 1$):** Whenever active hot keys in chunk $k$ fit within its 64-way associativity ($P_k \le 1$), the warmed set experiences zero capacity evictions (100.00% hit at $H_0=64$). When $P_k > 1$ under a uniform cyclic pass ($H_0=65$), 1-bit CLOCK immediately thrashes to 0.00% even though the remaining 15 chunks sit empty. Under random or skewed access with $P_k > 1$ (e.g., 128 keys in chunk 0), non-zero hit rates (32.56%) remain achievable because recently accessed subsets survive while unhit keys churn, though predictable residence fails.
2. **Occupancy Sensitivity Decomposition:** Comparing pure `100% Get` against mixed `80/20` demonstrates why throughput falls sharply between 1% fill (10 keys) and 50%–100% fill (512–1024 keys): at 1% fill, most chunks contain zero or one valid slot so SWAR tests return near-zero candidate matches (`94.81M` pure lookup, `67.56M` mixed). At 50%–100% fill (~32–64 active slots per chunk), misses and probes trigger more slot key comparisons (plus ~1/256 per-occupied-slot SWAR false positives) and active write-side CLOCK sweeps (`14.30M` mixed at 100% fill vs. `17.29M` pure `Get`).
3. **Scan Vulnerability & Bounded Redundant Writes:** A sequential scan exceeding capacity evicts 100% of hot items (`0/256`), whereas extreme single-key write contention converts 62.68% of redundant overwrites into bounded load-shed drops without deadlock.

### 9.12 Off-Heap Metadata vs. On-Heap Slices (GC Isolation)

To evaluate off-heap `mmap` metadata placement rigorously without process-level GC carry-over or struct-field mismatches, we constructed an isolated subprocess suite (`hardened_eval` Section 6, `65,536` slots = `8.1 MiB` of `slotState`/`chunk` metadata, identical 4-slice `atomic.Pointer` SoA fields in both variants, `runtime.KeepAlive` held through post-run `runtime.GC()`) across three regimes over equal **2.0-second windows** (8 workers):
1. **`GOGC=100` (Churn):** 80/20 `Get`/`Add` + background `512 B` heap allocation every 8 ops under default Go heap-growth pacing (`nextGC ≈ 2× liveHeap`).
2. **`GOMEMLIMIT=64 MiB` (`GOGC=off`, Churn):** Same 80/20 + `512 B` churn under a fixed 64 MiB soft memory ceiling (`debug.SetGCPercent(-1)` + `debug.SetMemoryLimit(64<<20)`).
3. **`Steady-State` (Zero-Alloc Lookup):** Pre-populated `65,536` slots, 100% `Get` (`0` allocations on hot path), 20 forced `runtime.GC()` cycles over 2 seconds to isolate metadata scanning cost.

| Experiment / Variant | Throughput | NumGC | Total Pause (Mean) | Post-GC `HeapAlloc` (`Inuse`) | Live `HeapObjects` (`Mid` -> `Live`) | `Mallocs` / `TotalAlloc` | `GCCPUFraction` |
|---|---:|---:|---:|---:|---:|---:|---:|
| **`GOGC=100` Off-heap `mmap`** | 20.36M ops/s | 202 | 33.84 ms (0.168 ms) | **6.17 MB** (6.72 MB) | 224,983 -> 195,987 | 5.69M / 2.49 GB | 0.16333 |
| **`GOGC=100` On-heap slices** | **23.86M ops/s** | **89** | **22.23 ms** (0.250 ms) | 14.24 MB (15.12 MB) | 279,463 -> 192,853 | 6.83M / 2.93 GB | **0.07072** |
| **`GOMEMLIMIT=64M` Off-heap** | 25.79M ops/s | **83** | **16.66 ms** (0.201 ms) | **6.17 MB** (7.15 MB) | 287,283 -> 196,041 | 7.46M / 3.18 GB | **0.06478** |
| **`GOMEMLIMIT=64M` On-heap** | 27.41M ops/s | 104 | 20.17 ms (0.194 ms) | 14.24 MB (15.15 MB) | 287,358 -> 192,722 | 7.93M / 3.36 GB | 0.07702 |
| **`Steady-State` Off-heap** | 45.60M ops/s | 20 | 5.63 ms (0.281 ms) | **6.10 MB** (6.56 MB) | `Live`: 192,421 | 36 / 0.00 MB | **0.01025** |
| **`Steady-State` On-heap** | 46.90M ops/s | 20 | 5.26 ms (0.263 ms) | 14.22 MB (14.62 MB) | `Live`: 192,421 | 40 / 0.01 MB | 0.01208 |

This controlled isolation clarifies both our earlier discrepancy and the true Go GC tradeoff:
1. **Resolving the `HeapObjects` count:** When both caches hold identical 4-slice payloads and stay live through post-run collection, live `HeapObjects` are **virtually identical** (`195,987` vs. `192,853`, and exact `192,421` vs. `192,421` in steady state), because Go slice metadata was two array allocations rather than 65,536 separate objects; our earlier Rev 3 count difference reflected unswept churn objects sampled between different GC cycle phases. Moving `states` and `chunks` off-heap instead removes **8.1 MB** (`57%`) of Go `HeapAlloc` (`6.10 MB` vs. `14.22 MB` at 64k slots).
2. **`GOGC=100` vs. `GOMEMLIMIT` inversion:** Under unconstrained small-heap `GOGC=100`, cutting `liveHeap` from `14.24 MB` to `6.17 MB` halves Go's automatic `2×` heap-growth trigger, so off-heap fires **2.27× more GC cycles** (`202` vs. `89`), raising total pause (`33.84 ms` vs. `22.23 ms`) and `GCCPUFraction` (`0.163` vs. `0.071`) despite shorter per-cycle pauses (`0.168 ms` vs. `0.250 ms`). Conversely, under a fixed **`GOMEMLIMIT=64 MiB` (`GOGC=off`)**, sparing `8.1 MB` of Go heap headroom reduces GC frequency by **20%** (`83` vs. `104`), cuts total STW pause by **17.4%** (`16.66 ms` vs. `20.17 ms`), and lowers `GCCPUFraction` by **15.9%** (`0.06478` vs. `0.07702`).
3. **Zero-alloc steady state:** Because `slotState` and `chunk` contain zero Go pointers, Go marks them in pointer-free `noscan` spans even when on-heap, so steady-state forced-GC pauses are comparable (`5.63 ms` vs. `5.26 ms`). Off-heap `mmap` metadata therefore reduces **Go heap footprint and `GOMEMLIMIT` GC pressure**, rather than lowering pointer-marking work or `GOGC=100` CPU cost on tiny heaps.

## 10. Discussion and Limitations

**Best-Effort, Non-Linearizable Admission & API Discoverability.** Because `Add` is not globally serialized across slots within a chunk, concurrent cold misses on the same key can insert duplicates in distinct slots, and `Get` always returns the lowest-index match rather than the most recently completed admission. Furthermore, future fast-path updates mutate only that lowest-index match; if it is later evicted, a stale duplicate at a higher index can resurface until reclaimed by CLOCK sweeps. Moreover, under pathological single-key contention (§9.11), `Add` silently sheds up to 62.68% of redundant overwrites (`drops++`) to bound retry work. Both the Go package documentation (`godoc` on `Add`, `Get`, and `Clear`) and callers must treat `liteLRU` explicitly as a **best-effort, bounded-staleness cache** for idempotent or memoized values rather than a linearizable concurrent map; applications requiring strict latest-write visibility must synchronize externally.

**Blocking `Clear()` Under Continuous Writes.** Unlike `Get` (bounded probes) and `Add` ($R=10$ bounded claim retries), `Clear()` waits via `runtime.Gosched()` and `CompareAndSwap(0, ^uint64(0))` for a zero `W_k` window per chunk. Continuous concurrent writes can starve `Clear()`, so callers needing deterministic teardown latency must quiesce writers externally.

**Memory Overhead.** The tradeoff for $L$-byte padded seqlocks and SoA alignment is increased memory overhead per entry compared to dense tag-based implementations like MemC3. On x86_64 ($L=64$), fixed per-slot arrays require 96 bytes/slot (64 bytes for `slotState` + 32 bytes for the four 8-byte `atomic.Pointer` SoA slices), totaling **~96 MB** per million slots before heap-allocated key/payload objects (`*string` wrappers, strings, and `[]Param`). On Apple Silicon ($L=128$), the 128-byte `slotState` padding brings fixed array overhead to 160 bytes/slot (**~160 MB** per million slots before chunk metadata and key/payload objects). Both represent a memory premium in exchange for lock-free bounded-probe reads, with `otter` requiring **~120 MB** per million entries.

**Hit Rate vs. True LRU and S3-FIFO.** The Chunked Bitmask algorithm is a 1-bit CLOCK approximation of LRU, not true LRU or scan-resistant S3-FIFO. As demonstrated in §9.6 and §9.11, sequential scans larger than capacity evict hot entries (`0/256`), workloads exceeding 64 active keys in a single set suffer collision evictions, and S3-FIFO outperforms 1-bit CLOCK at lower capacities or flatter Zipf skew ($s=0.8$).

**Fixed Capacity.** The cache does not resize and rounds requested capacities up to powers of two (`nextPowerOfTwo`, minimum 64). While this eliminates steady-state resizing allocations, callers must account for power-of-two capacity rounding when provisioning memory.

**Extreme Contention Load-Shedding & Scoped Bounds.** To bound ownership-claim work, `liteLRU` limits CAS retries in `findVictim` and per-matched-slot `W_k` acquisition to $R=10$ attempts, and sheds redundant updates when a slot's `W_k` bit is already held. Post-claim signature-word CAS (`chk.sigs[sigWordIdx]`) lies outside $R$ (Theorem 2). Under pathological single-key upsert contention (§9.11), `liteLRU` sheds ~62.68% of redundant writes to prevent unbounded claim spinning.

**Zero-Allocation Scope.** `Get` guarantees **0 heap allocations** (verified via `testing.AllocsPerRun`) on cache misses and on cache hits whenever the caller supplies a `dst` slice with `cap(dst) >= len(params)` (including `nil` when `len(params) == 0`). By contrast, `Add` allocates immutable `*string` / `*HandlerFunc` wrappers on miss admissions (and fresh `[]Param` copies when `len(params) > 0`) to preserve single-word race-free `atomic.Pointer` publication.

---

## 11. Conclusion

We have presented the hardware-grounded derivation of each principal design decision in `liteLRU`: a hybrid memory architecture isolating off-heap synchronization metadata (`slotState`, `chunk` bitmasks, and SWAR signatures) to cut Go `HeapAlloc` footprint under `GOMEMLIMIT` constraints while safely tracking immutable heap payloads via single-word `atomic.Pointer` SoA slices [[9,15]](#ref-gogc); SoA layout to maximize cache-line occupancy per field access [[7]](#ref-soa); bitset-encoded recency state enabling bulk $O(1)$ candidate evaluation via hardware `CTZ` intrinsics [[10,11]](#ref-tzcnt); monotonic 64-bit seqlocks [[14]](#ref-seqlock) with $L$-byte cache-line padding [[1,8]](#ref-mesi) across `slotState` and `statStripe` structs; and explicit $W_k$ ownership serialization with $R=10$ per-claim load-shedding. Across 10-run hardened evaluations and semantic-equivalence controls (§9.1, §9.4, §9.7), `liteLRU` trails `otter v1.2.4` on ultra-read-dominated insert-on-miss caching (`33.64M` vs. `94.27M` ops/s at 99.94% hits) and yields lower hit rates under small-capacity flat Zipf skew (§9.6), while outperforming `otter` (`1.17×–4.89×`) and `Mutex LRU` (`3.02×–4.32×`) once eviction churn or concurrent writes dominate, with 1.69 µs sampled p99 latency on the uniform baseline.

---

## References

<a name="ref-mesi"></a>
[1] Papamarcos, M. S., & Patel, J. H. (1984). A low-overhead coherence solution for multiprocessors with private cache memories. *ISCA '84*.

<a name="ref-amdahl"></a>
[2] Amdahl, G. M. "Validity of the Single Processor Approach to Achieving Large Scale Computing Capabilities." *AFIPS Spring Joint Computer Conference*, pp. 483–485, 1967. https://doi.org/10.1145/1465482.1465560

<a name="ref-clock"></a>
[3] Corbató, F. J. "A Paging Experiment with the Multics System." *In Honor of Philip M. Morse*, MIT Press, pp. 217–228, 1969. (Foundational 1-bit CLOCK algorithm.)

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
[13] Jiang, S., Chen, F., and Zhang, X. "CLOCK-Pro: An Effective Improvement of the CLOCK Replacement." *USENIX Annual Technical Conference*, 2005. (Scan-resistant multi-hand extension distinct from foundational CLOCK [3].)

<a name="ref-seqlock"></a>
[14] Lameter, C. "Effective Synchronization on Linux/NUMA Systems." *Gelato Conference*, 2005. https://lameter.com/gelato2005.pdf

<a name="ref-gomem"></a>
[15] The Go Authors. "The Go Memory Model." 2024. https://go.dev/ref/mem

<a name="ref-memc3"></a>
[16] Fan, B., Andersen, D. G., and Kaminsky, M. "MemC3: Compact and Concurrent MemCache with Dumber Caching and Smarter Hashing." *USENIX NSDI*, 2013. https://www.cs.cmu.edu/~dga/papers/memc3-nsdi2013-abstract.html

<a name="ref-dice"></a>
[17] Dice, D., Kogan, A., and Lev, Y. "Understanding and Improving the Performance of Concurrent Applications." *USENIX*, 2013.

<a name="ref-otter"></a>
[18] Otter Authors. "Otter: A high performance concurrent cache in Go (`v1.2.4`)." *GitHub*, 2023. https://github.com/maypok86/otter

## 12. Reproducibility

* **Platform Architecture**: Apple Silicon M2 (8 cores: 4 performance + 4 efficiency cores; $L=128$). At `GOMAXPROCS=8`, Go goroutine scheduling across heterogeneous P/E cores introduces natural multi-run variance (§8, §9.2).
* **Go Version**: `go1.25.7 darwin/arm64`
* **macOS Version**: macOS 14.5
* **Machine RAM**: 24GB (`hw.memsize = 25769803776`)
* **Hardened Codebase Measurements**: §§8 (`BenchmarkParallelLRUCache`, 5 runs), 9.1, 9.2, 9.4, 9.6, 9.7 (10 runs each via `benchmarks/hardened_eval`, `zipf_bench.go`, `zipf_low_skew_bench.go`, reporting median and IQR), §9.3 (`TestConcurrentLatency`, 10 uncached runs), and §§9.11–9.12 (`adversarial_bench.go`, `hardened_eval` Section 6) were all measured on the hardened race-free `atomic.Pointer` / `W_k`-serialized implementation.
* **Historical HTTP Snapshot**: §§9.8–9.10 preserve the earlier pre-hardening `vegeta` HTTP snapshot from commit `f826423b006c4df62cb1fc1ddfcf76eb2e9b0682` (with `server.go` requesting `capacity=75000`, which rounded to `131,072` slots for `liteLRU` vs. `75,000` for Otter; note `benchmarks/ablation/` also preserves an earlier hash-map ablation harness).
* **Comparator**: `github.com/maypok86/otter v1.2.4`
* **Environment**: `GOMAXPROCS=8`
* **Commands**:
  - Race & correctness verification: `go test -v -race ./...`
  - Latency & parallel Go harnesses: `go test -v -count=10 -run=TestConcurrentLatency .` and `go test -run=^$ -bench=BenchmarkParallelLRUCache -benchmem -cpu=1,2,4,8 -count=5 .`
  - Unified hardened evaluation (`cd benchmarks`): `go run ./hardened_eval` (or scoped runs `--only-baseline`, `--only-ablation`, `--only-write`, `--only-gc`), `go run adversarial_bench.go`, `go run zipf_bench.go`, `go run zipf_low_skew_bench.go`
