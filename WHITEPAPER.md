<div align="center">

# liteLRU: A Coherence-Conscious, Contention-Bounded Cache for Managed Runtimes via Chunked Bitmasks

**Author:** xDarkicex  
**Affiliations:** libravdb · bitdev · LibraVDB, INC.  
**Contact:** git@libravdb.com  
**Repository:** https://github.com/xDarkicex/liteLRU

---


</div>

## Abstract

Concurrent approximate LRU structures often serialize cores on shared metadata. We investigate whether a bounded-probe, 64-way set-associative cache combining bit-sliced S3-FIFO eviction, single-pointer immutable payloads, and contention-aware admission load-shedding can achieve both high scan-resistant hit rates and predictable concurrency costs without centralized locking in a garbage-collected runtime. We present `liteLRU` (v2), a fixed-capacity cache in Go whose compact synchronization metadata (SWAR signatures, Small/Main partitions, 2-bit bit-sliced frequencies, 128-bit Ghost Bloom filter, and writer ownership masks) packs into 128-byte off-heap `mmap` chunks (`2 bytes/slot`), while key/value snapshots reside on the Go heap behind a single `atomic.Pointer[cacheEntry]` per slot (`8 bytes/slot`). Reads provide lock-free bounded-probe execution with zero heap allocations on hits when supplied a caller buffer and never mutate that buffer on misses; writes are best-effort, non-linearizable cache insertions/updates capping per-slot ownership CAS retries at $R=10$. Across 10-run hardened evaluations on an 8-core Apple M2 (24 GB RAM), `liteLRU` sustains **51.32M ops/s** on the uniform 80/20 baseline (**3.14×** `otter v1.2.4` and **7.29×** `Mutex LRU`, with 500 ns sampled p99 vs. 9.90 µs for `otter`), leads `otter` across both immutable insert-on-miss workloads (**128.59M vs. 113.74M ops/s** at $N=1\text{k}$; **56.29M vs. 9.12M ops/s** at `cap=16384, N=100k`) and direct overwrite contention (**26.57M–32.51M effective ops/s**, **5.33×–6.58×** `otter`), achieves higher matched-capacity Zipfian hit rates than `otter` across all six `(s, cap)` points (`+0.05` to `+2.38 pp`), and delivers a **1.30×** end-to-end throughput advantage under an explicit 5 µs backend miss penalty (§9.1, §9.6, §9.7).

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

Set associativity, SWAR signatures, S3-FIFO queues, and stack-sharded counters are established systems building blocks; `liteLRU` contributes their lock-free, bit-parallel integration for managed runtimes. Specifically, we investigate:

> *Can a bounded-probe, 64-way set-associative cache using bit-sliced S3-FIFO state in 128-byte chunks, single-pointer immutable entries, and stack-sharded telemetry simultaneously deliver lock-free throughput, scan resistance, and compact metadata in Go?*

We separate the resulting design into three orthogonal guarantees—**Safety** (no torn payloads, invalid pointer exposure, unsynchronized mutation, or failed-read buffer corruption), **Progress** (lock-free bounded-probe reads and retry-bounded, best-effort writes), and **Performance** (minimal L1D footprint, write-avoiding frequency promotion, and stack-sharded telemetry)—and contribute:

* **Bitmask S3-FIFO within 64-Way Chunks:** A complete lock-free S3-FIFO eviction engine (Small/Main partition mask `prob`, 2-bit bit-sliced saturating frequencies `f0`/`f1`, 128-bit resetting Ghost Bloom filter `ghost0`/`ghost1`, and dual 6-bit rotating hands `hand`/`handM`) packed alongside 64 byte signatures inside a single 128-byte off-heap `chunk` (`2 bytes/slot`), using hardware `CTZ` intrinsics instead of pointer-based FIFO queues or global clock hands.
* **Single-Pointer Immutable Entries (Seqlock Elimination):** Collapsing the earlier 128-byte padded per-slot seqlocks and 4-slice SoA arrays into one `entries []atomic.Pointer[cacheEntry]` array (`8 B/slot`) pointing to immutable 64-byte `cacheEntry` snapshots (`10 B/slot` total fixed metadata vs. `162 B/slot` in v1 on ARM64). This eliminates multi-array L1D misses, seqlock retries, failed-read `dst` mutation, and any 64-bit generation wraparound hypothesis.
* **Sub-Line Layout & Stack-Sharded Telemetry:** Splitting read-mostly `sigs` `[0..64)` from mutating control bitmasks `[64..128)` inside every `chunk`, and sharding hit/miss/drop counters across 128 cache-line-padded `statStripe` slots indexed by 2 KB-aligned goroutine stack pointers (`sp >> 11`) rather than key hashes—preventing cross-core counter ping-pong on skewed hot keys.
* **Load-Shedding Best-Effort Admission Protocol:** A contention-bounded, non-linearizable write protocol that caps per-claim ownership CAS retries on `W_k` to $R=10$ attempts (with post-claim signature-word CAS retries scoped outside $R$); under sustained contention, redundant updates or admissions are shed cleanly rather than spinning unboundedly.

The rest of this paper is structured as follows: Section 2 details the hybrid memory layout and SWAR filtering, Section 3 discusses related work, Section 4 explains the control-plane vs. payload layout tradeoff, and the remaining sections analyze the Bitmask S3-FIFO algorithm, correctness, cache-line isolation, and evaluation.

## 2. Hybrid Memory Architecture and GC Isolation

**Scanner Heap Pressure & Payload Density.** Objects holding Go pointers escape to the heap and are traced by the garbage collector. An earlier `liteLRU` v1 prototype maintained off-heap `slotState` seqlocks (`128 B/slot` on ARM64) plus four separate heap `atomic.Pointer` arrays (`methods`, `paths`, `handlers`, `params` = `32 B/slot`), which bloated fixed metadata to `162 B/slot` and required up to four separate wrapper allocations and dereferences per entry.

`liteLRU` v2 pairs compact off-heap control chunks (`2 B/slot`) with single-pointer immutable `cacheEntry` snapshots (`8 B/slot` array overhead) through three architectural choices:

1. **64-Way Set Associativity over Hash Maps**: Prior open-addressed designs considered for `liteLRU` require tombstones or epoch reclamation to preserve concurrent probe sequences. `liteLRU` instead overwrites a victim in place within a fixed 64-slot bucket (a chunk selected by `HashRoute(method, path) & (numGroups - 1)`), using word-at-a-time 8B/4B string mixing (`mixString`) for deterministic zero-allocation hashing.
2. **SWAR Signature Scanning**: To filter candidate slots within a 64-slot set before dereferencing heap `cacheEntry` pointers, each `chunk` begins (`bytes [0..64)`) with `[8]atomic.Uint64` `sigs`, packing 64 non-zero 8-bit signatures (`sig8 = uint8(hash >> 32)`, clamped from `0` to `1` so empty `0` signatures are never matched). SWAR bitwise arithmetic tests 8 byte signatures per machine word in parallel, gated by valid bits `validBits`. A non-zero match byte (`255` active values $\implies \approx 1/255$ random false-positive rate per valid slot, $\sim 3.1\%$ across an 8-slot word) triggers one `atomic.Pointer[cacheEntry]` load and full `(method, path)` string comparison; false positives never cause incorrect responses.
3. **Micro-Metadata Off-Heap vs. Immunized Payloads**: Every `chunk` occupies exactly `128 bytes` (`sigs [8]atomic.Uint64` at `[0..64)` and control bitmasks + hands at `[64..128)`) inside a page-aligned anonymous `mmap` slab. Thus `sigs` occupies a single `64B` x86\_64 line (or shares the single `128B` Apple Silicon chunk line with control masks) without occupying Go heap spans. Conversely, `entries []atomic.Pointer[cacheEntry]` remains on the Go heap so the GC safely traces live `method`/`path` strings, `HandlerFunc` closures, and `[]Param` slices without manual reclamation hazards. Unlike v1—where moving `8.1 MiB` of per-slot seqlocks off-heap altered `GOGC=100` collection pacing by 2.27×—eliminating seqlocks leaves only `128 KiB` of chunk control state per 65,536 slots, making Go runtime memory overhead near-minimal regardless of workload (§9.12).

---

## 3. Related Work

Concurrent cache design spans two complementary axes: the eviction *policy* governing hit rates, and the concurrent *mechanism* governing synchronization cost. `liteLRU` v2 advances the mechanism axis—executing **both** lookup filtering and S3-FIFO state transitions via lock-free 64-bit bitmasks without locks, global hands, or background eviction threads.

**Concurrent CLOCK Mechanisms.** The closest published system to `liteLRU` is MemC3 [[16]](#ref-memc3), which optimized Memcached by introducing a concurrent CLOCK with 1-bit recency tags and optimistic locking to remove mutexes from the read path. Our v1 prototype built on MemC3 in Go using off-heap seqlocks and bulk `CTZ` scans over 1-bit CLOCK masks, while `liteLRU` v2 replaces per-slot seqlocks with single immutable `atomic.Pointer` snapshots and upgrades 1-bit CLOCK to multi-bit bit-sliced S3-FIFO inside the same 128-byte chunk.

**S3-FIFO & Scan-Resistant Policies.** Recent eviction policies such as CAR, CLOCK-Pro [[13]](#ref-clockpro), S3-FIFO [[5]](#ref-s3fifo), and SIEVE [[6]](#ref-sieve) prevent one-hit scans from flushing hot working sets. Production S3-FIFO caches like `otter` [[18]](#ref-otter) implement Small, Main, and Ghost FIFO queues using linked-ring buffers, mutex-guarded eviction queues, striped read buffers, and asynchronous background ring draining. `liteLRU` v2 contributes a **bit-sliced, lock-free hardware-intrinsic realization of S3-FIFO**: Small/Main membership (`prob`), 2-bit saturating frequencies (`f0`, `f1`), Small eviction hand (`hand`), Main eviction hand (`handM`), and a 128-bit resetting Ghost filter (`ghost0`, `ghost1`) are all compacted into bitmasks inside a single 64-slot `chunk`, advancing in place via `CTZ` without background goroutines or write queues.

**Production Go Caches.** Existing Go caches like `freecache`, `bigcache`, and `ristretto` achieve scalability via sharding, serialization, or Count-Min Sketch batching in background goroutines. `liteLRU` executes reads purely lock-free within bounded probes and executes inline best-effort admissions bounded by $R=10$ claim retries.

---

## 4. Control-Plane Bit-Slicing vs. Single-Pointer Entry Snapshots

*Note on Set Associativity:* Software caches traditionally use open-addressed hash maps, whereas hardware L1/L2 caches use $N$-way set associativity. `liteLRU` applies this hardware-style set associative design in software to eliminate global hash-table locking and tombstone chains entirely.

In our v1 prototype, we separated the four entry payload fields (`methods`, `paths`, `handlers`, `params`) into four separate heap `atomic.Pointer` arrays (`SoA` at `32 B/slot`) protected by an off-heap `slotState` seqlock array (`128 B/slot` on ARM64). Empirical ablation and microarchitectural analysis exposed a clear tradeoff between **control-plane** SoA and **payload** SoA:

```go
// v1 payload SoA (32 B/slot across 4 arrays + 128 B/slot seqlocks = 160 B/slot)
methods  []atomic.Pointer[string]
paths    []atomic.Pointer[string]
handlers []atomic.Pointer[HandlerFunc]
params   []atomic.Pointer[[]Param]

// v2 split: bit-sliced control plane off-heap (2 B/slot) + single-pointer AoS snapshot on heap (8 B/slot)
type cacheEntry struct { // 64 bytes immutable snapshot on Go heap
    method  string
    path    string
    handler HandlerFunc
    params  []Param
}
entries []atomic.Pointer[cacheEntry] // 8 pointers per 64B line, 16 per 128B line
```

1. **Bit-Sliced Control Plane (Where SoA Wins Big):** Placing signatures (`sigs`) and recency/frequency state (`valid`, `prob`, `f0`, `f1`, `writing`, `ghost0`, `ghost1`) in contiguous off-heap `uint64` masks yields up to **64× bit-level packing**. Eviction aging, Small-to-Main promotion (`probBits & f0`), and candidate extraction (`CTZ`) touch only a single 128-byte `chunk` rather than fetching 64 entry structs.
2. **Single-Pointer Immutable Payloads (Why Payload Collapsing Wins in Go):** In Go, `*atomic.Pointer[string]` cannot hold raw strings inline without losing atomic publication safety: every v1 lookup paid a SWAR line fetch, **two** `slotState.seq` fetches (`128 B/slot`, spanning `8 KiB` per chunk), **four** separate `atomic.Pointer` array fetches, plus string-header dereferences. Collapsing all four payload fields into a single immutable 64-byte `cacheEntry` referenced by one `atomic.Pointer[cacheEntry]` packs 8 slot pointers per 64B cache line (`16` per 128B line), shrinks fixed per-slot arrays by **16×** (`10 B/slot` total with `chunk` share vs. `162 B/slot` in v1 on ARM64), turns 4 separate `Add` wrapper allocations into 1 `cacheEntry` allocation, and removes the `Get` seqlock retry loop altogether.

Formally, for $n$ matching candidate slots within a chunk, v2's `Get` fetches $\lceil (a + n \cdot 8\text{ B}) / L \rceil$ lines from `entries` plus only the single immutable `cacheEntry` whose SWAR `sig8` matched, reducing L1D pressure substantially when `sigs` eliminates ~99.6% of non-matching slots before any `entries[idx]` dereference occurs.

---

## 5. Bitmask S3-FIFO: From One Bit to Bit-Sliced Chunk Queues

### 5.1 The Cost of Pointer-Based Queues

Traditional LRU [[4]](#ref-lru) maintains doubly-linked lists requiring 8 pointer writes per promotion behind a mutex, while canonical S3-FIFO [[5]](#ref-s3fifo) maintains three concurrent queues (Small FIFO, Main FIFO, Ghost FIFO). Protecting linked structures with mutexes serializes accesses, and lock-free pointer queues require epoch-based reclamation or background drainer goroutines to reclaim removed nodes safely [[12]](#ref-lockfree).

### 5.2 Wait-Free Frequency Filters & Scan Resistance

In v1, each chunk used a single bit $A_k$ (1-bit CLOCK [[3]](#ref-clock)). While fast, 1-bit CLOCK lacks scan resistance: a sequential scan exceeding capacity flushes every hot entry in a chunk (`0/256` retained, §9.11), and a cyclical 65-key loop within one 64-slot chunk collapses to a `0.00%` hit rate.

### 5.3 Bit-Sliced S3-FIFO Inside 128-Byte Chunks

`liteLRU` v2 embeds an entire **S3-FIFO** replacement policy inside each 64-slot chunk's second 64 bytes (`[64..128)`) via orthogonal 64-bit masks and two 6-bit rotating hands (`hand`, `handM`), preserving constant-width `CTZ` execution with **zero** heap pointers:

- $V_k \in \{0,1\}^{64}$ (`valid`): bit $i = 1$ iff slot $i$ holds a published entry.
- $P_k \in \{0,1\}^{64}$ (`prob`): bit $i = 1$ iff valid slot $i$ belongs to the **Small** probationary queue; $M_k = V_k \land \neg P_k$ is the **Main** protected queue.
- $(F_{1,k}, F_{0,k}) \in \{0,1\}^{64} \times \{0,1\}^{64}$ (`f1`, `f0`): 2-bit bit-sliced saturating thermometer frequency ($00 \to 01 \to 11$). On reads, `promoteFreq` skips any atomic write if the slot's bit is already set, turning repeated hot hits into pure shared reads without RFO invalidations.
- $W_k \in \{0,1\}^{64}$ (`writing`): bit $i = 1$ iff a writer exclusively owns slot $i$.
- $(G_{0,k}, G_{1,k})$ (`ghost0`, `ghost1`): a 128-bit double-hashed Ghost Bloom filter (`(h>>40)&63` in `ghost0` and `(h>>48)&63` in `ghost1`), automatically reset when `popcount(ghost0) >= 34`.
- `hand`, `handM` $\in \{0 \dots 63\}$: rotating 6-bit starting offsets for Small and Main circular selection (`pickFromHand`).

When an eviction is required (`findVictim(k)`), candidate selection operates purely via register-width bitmasks and hardware `CTZ` [[10,11]](#ref-tzcnt) (`smallTarget = 4`, reserving ~6.25% of each chunk for new admissions):
1. **Empty slots first:** $\text{empty} = \neg V_k \land \neg W_k$. If non-zero, claim `CTZ(empty)`.
2. **Bulk $O(1)$ Small $\to$ Main Promotion:** Evaluate `hitSmall = (prob & valid) & f0`. Any Small slot touched at least once (`f0=1`) is promoted into Main across all 64 slots simultaneously in three atomic bit-clear ops (`prob.And(^hitSmall)`, `f0.And(^hitSmall)`, `f1.And(^hitSmall)`), resetting frequency to `00` upon entering Main just as in S3-FIFO.
3. **Small FIFO Eviction:** If `popcount(prob) >= smallTarget`, compute `coldSmall = prob & ^f0 & ^W_k`; select `pickFromHand(coldSmall, hand)` (`CTZ` above `hand`, wrapping around `0`), claim $W_k[i]: 0 \to 1$ via CAS, and advance `hand`.
4. **Main FIFO Frequency-Decrementing Eviction:** Otherwise compute `mainBits = valid & ^prob` and scan `coldMain = mainBits & ^f0 & ^W_k` from `handM`. If no zero-frequency Main slot exists, age **only Main entries** in $O(1)$ (`(f1, f0) -> (0, f1)` via `f0.And(^mainBits | f1); f1.And(^mainBits)`), leaving Small frequencies untouched. If Small holds all 64 slots simultaneously (`mainBits == 0`), clear `f0` and evict from Small via `hand`.
5. **Ghost Tracking & Bypass:** Evicting an unhit Small slot records its key hash into `(ghost0, ghost1)`. Subsequent `Add` probes `(ghost0, ghost1)`: a Ghost hit admits directly to Main (`prob[i] = 0`); cold misses enter Small (`prob[i] = 1`).

**Load-Shedding Eviction.** Just as in v1, `findVictim` caps unsuccessful iterations at $R=10$ CAS/aging retries, returning sentinel `0xFFFFFFFF` to shed admission load under extreme single-chunk saturation.

### 5.4 Eliminating Global Locks and Shared Hands

Because `liteLRU` hashes each key to a fixed 64-slot chunk $k = h(r) \bmod N$, both `Get` and `Add`/`findVictim` operate **strictly within chunk $k$**. Small FIFO queues, Main CLOCK-with-frequency queues, Ghost Bloom filters, and both rotating hands are entirely local to each 128-byte chunk, removing cross-chunk synchronization while exposing set-associative capacity skew ($P_k > 1$, §9.11).

---

## 6. Algorithm and Correctness

### 6.1 State Definitions and Layout

The cache state separates global bounds, off-heap per-chunk control planes, heap entry snapshots, and stack-sharded counters:

* **Global:** Total power-of-two capacity $C \ge 64$, chunk count $N = C/64$, and stack-sharded `stats [128]statStripe`.
* **Per-Chunk (off-heap `mmap`, `128 B`):** Sub-line 0 `[0..64)` holds `sigs [8]atomic.Uint64`; Sub-line 1 `[64..128)` holds `valid`, `prob`, `f0`, `f1`, `writing`, `ghost0`, `ghost1`, `hand`, and `handM` (`2 bytes/slot`).
* **Per-Slot (Go heap, `8 B/slot`):** `entries []atomic.Pointer[cacheEntry]`, where each `cacheEntry` (`64 B`) stores an immutable `(method, path, handler, params)` snapshot created once at admission/update and never modified in place.

### 6.2 API Semantics, State Machine, and Control Flow

**Best-Effort, Non-Linearizable Cache Semantics and Duplicate Keys.** `Add(method, path, handler, params)` is a **best-effort, non-linearizable cache insertion/update**, *not* a globally serialized upsert:
1. If an existing valid entry for `(method, path)` matches during the chunk SWAR scan, `Add` acquires $W_k[i]: 0 \to 1$ and replaces `entries[group*64+i]` with the newly constructed immutable `*cacheEntry` snapshot via single-word `atomic.Pointer.Store`. If $W_k[i]=1$ is already held, `Add` sheds the redundant write (`drops++`) and returns.
2. On a miss, `Add` claims a victim slot via `findVictim(k)`. Simultaneous cold misses on the same key by writers holding distinct $W_k$ bits can still insert duplicate entries in distinct slots (e.g., Writer A in slot 4 with $v_A$, Writer B completing later in slot 12 with $v_B$). Because `Get` scans slots $0\dots 63$ in deterministic ascending order and returns the first match, `Get` returns $v_A$ even though Writer B completed later, and subsequent updates target only slot 4; evicting slot 4 later can briefly resurface slot 12 (**stale duplicate resurrection**) until evicted by S3-FIFO.
3. **Read Semantics & `dst` Non-Mutation Guarantee:** Key verification (`ep.method == method && ep.path == path`) occurs strictly **before** touching caller slice `dst`. Consequently, `Get` guarantees **zero mutation of `dst` on a miss** (verified by `TestZeroAllocGet`). Note that `Get` may transiently return a miss even if a key was recently resident whenever an in-flight writer has cleared $V_k[i]$ (`chk.valid.And(^mask)`) while overwriting that slot, which is permissible under the best-effort contract.

**Slot Publication State Machine.** Each slot transitions across **INVALID** ($V_k[i]=0$), **WRITING** ($W_k[i]=1$), and **VALID** ($V_k[i]=1$):
* **Eviction/Insert (`Add`):** Acquire $W_k[i]: 0 \to 1$ in `findVictim`; clear $V_k[i] \leftarrow 0$; if replacing an unhit Small entry (`prob[i]=1`), record evicted hash in `(ghost0, ghost1)`; publish `entries[idx].Store(newEntry)`; CAS `sig8` into `sigs[bit/8]`; set Small/Main bit `prob` via Ghost check; clear `f0[i]`, `f1[i]`; set $V_k[i] \leftarrow 1$ (visibility linearization point); and release $W_k[i] \leftarrow 0$.
* **Fast-Path Update (`Add`):** Under $W_k[i]=1$, verify $V_k[i]=1$ and key identity, publish `entries[idx].Store(newEntry)`, call `promoteFreq(chk, mask)`, and release $W_k[i] \leftarrow 0$.

**Get(method, path, dst)**
1. Compute 64-bit `hash = HashRoute(method, path)` using word-at-a-time mixing, chunk $k = \text{hash} \bmod N$, and non-zero `sig8`.
2. Load `validBits = chk.valid.Load()`; if `0`, increment `stats[workerStripe()].misses` and return miss.
3. For SWAR word $i = 0 \dots 7$, skip if `vByte == 0`; otherwise evaluate SWAR byte equality against `uint64(sig8) * 0x0101010101010101` (`matches`).
4. For each set match bit via `bits.TrailingZeros64(matches)`, verify `(vByte & (1<<j)) != 0` and exact byte equality `uint8(word>>(j*8)) == sig8`, then load `ep = c.entries[idx].Load()`.
5. If `ep != nil && ep.method == method && ep.path == path`, copy `ep.params` into `dst` (zero-alloc when `cap(dst) >= len(ep.params)`), call write-avoiding `promoteFreq(chk, 1<<bit)`, increment `stats[workerStripe()].hits`, and return `(ep.handler, copiedParams, true)`.
6. Otherwise increment `stats[workerStripe()].misses` and return `(nil, nil, false)`.

**Clear()**
1. Iterate sequentially over chunks $k = 0 \dots N-1$.
2. Poll $W_k = 0$ (yielding via `runtime.Gosched()`) and acquire all 64 ownership bits atomically via `CompareAndSwap(W_k, 0, ^uint64(0))`.
3. Store `0` atomically to `valid`, `prob`, `f0`, `f1`, `ghost0`, `ghost1`, `hand`, `handM`, and `sigs[0..7]`; store `nil` to `entries[group*64 + 0..63]` for GC reclamation; release `chk.writing.Store(0)`. Zero all 128 `statStripe` counters once all chunks finish.

*Scope & Progress Note on `Clear()`*: `Clear()` guarantees per-chunk exclusive clearing, **not** globally atomic whole-cache invalidation: concurrent writers can insert into chunk `0` after `Clear()` releases it while chunk `1` is being cleared. Furthermore, `Clear()` is blocking and **not starvation-free** under continuous concurrent writes; callers requiring whole-cache atomic resets or deterministic teardown under write load must quiesce writers externally.

**Linearization Points and Stale Mask Snapshots.** Because `findVictim` loads `valid`, `prob`, `f0`, and `writing` in separate atomic reads before executing CAS on $W_k$, a reader may promote a frequency bit between the load and the $W_k$ claim, or clear/reset `ghost0` concurrently. As with 1-bit CLOCK, stale recency/frequency observations affect only approximate eviction preference, whereas exclusive slot ownership is linearized strictly at the successful $W_k$ CAS. Once $W_k[i]=1$ is held, no other writer or `Clear()` can mutate slot $i$, $V_k[i]$, or `entries[idx]` until $W_k[i]$ is released.

### 6.3 Go Memory Model Safety

Because multi-word Go values (16-byte `string` headers, 24-byte `[]Param` headers) can tear if mutated in place, and because v1's per-slot seqlocks required post-copy generation validation, `liteLRU` v2 enforces safety cleanly through single-word immutable snapshot publication:
1. Every `*cacheEntry` is initialized cleanly (with a freshly copied `[]Param` if non-empty) *before* publishing its pointer via `entries[idx].Store(newEntry)` and is **never mutated afterward**.
2. In `Add` eviction/insert, `entries[idx].Store(newEntry)` strictly precedes both the `chk.sigs` CAS and `chk.valid.Or(mask)` visibility store under Go's sequentially consistent `sync/atomic` total order [[15]](#ref-gomem). Thus any reader observing `newSig` after `entries[idx].Store(newEntry)` (or matching `ep` directly) loads a complete immutable object kept alive by the Go garbage collector until the reader finishes dereferencing it.
3. All writers to slot $i$ (`Add` fast path, `Add` eviction, `Clear`) serialize exclusively via $W_k[i]$.

### 6.4 Correctness Theorems

**Theorem 1 (Bounded-Probe Lock-Free Reads).** *Any thread executing `Get` terminates in a bounded number of metadata probes (at most 8 SWAR words and 64 slots; per-candidate key comparison cost is $\mathcal{O}(|\text{key}|)$), regardless of the state or execution speed of concurrent writers.*

*Proof:* `Get` contains no loops waiting on writer state or seqlock retries. It evaluates `chk.valid`, at most 8 `chk.sigs` words (`vByte != 0`), and at most 64 candidate indices (`bits.TrailingZeros64`), performing one `atomic.Pointer[cacheEntry]` load and at most one key comparison per matching signature before advancing or returning hit (with at most one conditional `Or` on `f0`/`f1`). $\square$

**Theorem 2 (Bounded Ownership-Claim Retries).** *Any `Add` execution decomposes as $T_{\mathrm{Add}} = T_{\mathrm{lookup}} + T_{\mathrm{claim}} + T_{\mathrm{publication}}$, where ownership-claim CAS retries on $W_k$ satisfy $N_{\mathrm{claim,retries}} \le R = 10$ per matched fast-path slot and $N_{\mathrm{claim,retries}} \le R = 10$ inside `findVictim` (so an operation encountering $d \ge 0$ matching duplicate slots performs at most $R$ claim retries per matched slot plus at most $R$ in `findVictim`), whereas post-claim signature-word publication ($T_{\mathrm{publication}}$) lies outside $R$. Conversely, `Clear()` is blocking and non-atomic across chunks.*

*Proof:* Every loop iteration inside `findVictim` (failed `W_k` CAS or `f0`/`f1` aging pass) increments `retries`, returning `0xFFFFFFFF` at `retries == 10`. On fast-path updates, each matched duplicate slot $i$ retries `W_k` CAS at most 10 times on unrelated-bit interference, dropping immediately if $W_k[i]=1$ or when `retries+1 >= 10`. Outside $R$ lie OS scheduling, `cacheEntry` / `[]Param` allocations, iterating up to $d$ duplicate matches if invalidated mid-claim, and the post-claim `chk.sigs[bit/8].CompareAndSwap` loop (which only retries when another writer simultaneously modifies a *different* slot within the same 8-slot signature word). $\square$

**Theorem 3 (Unconditional Snapshot Safety & Clean Misses).** *No two writers may simultaneously mutate the same slot; every accepted read in `Get` observes a complete, untorn `(method, path, handler, params)` snapshot from a single publication without any 64-bit generation-wraparound assumption; and no failed lookup or miss mutates caller buffer `dst`.*

*Proof:* Writer exclusion (`Add` fast path, `Add` eviction, `Clear`) is enforced strictly by $W_k[i]$ (`0 -> 1` CAS or all-ones `CompareAndSwap(0, ^uint64(0))` in `Clear`). Every `entries[idx].Store(newEntry)` publishes a single-word `atomic.Pointer[cacheEntry]` whose target struct and `[]Param` backing array are immutable for their entire lifetime, so any concurrent `entries[idx].Load()` returns either `nil` or a live, complete, immutable snapshot under Go's memory model (requiring zero generation counters). Finally, `Get` completes full key verification (`ep.method == method && ep.path == path`) before writing `copiedParams` into `dst[:len(ep.params)]`, guaranteeing `dst` remains untouched on every miss or failed probe. $\square$

## 7. Coherence-Line Layout and Stack-Sharded Telemetry

In `liteLRU` v1, 128-byte padded `slotState` seqlocks isolated concurrent write counters per slot at the cost of `8 KiB` of L1D footprint per chunk—causing unpadded `mmap` seqlocks to beat padded ones by `+4.8%` on read-only lookups (`Regime A0`) while gaining only `+1.1%` to `+5.9%` under eviction. Eliminating `slotState` altogether frees the design to apply targeted coherence-line separation only where read/write traffic actually collides:

1. **Sub-Line `chunk` Partitioning (`128 B`):** `TestLayoutInvariants` verifies `sizeof(chunk) == 128`, `offsetof(chunk.sigs) == 0` (`64 B`), and `offsetof(chunk.valid) == 64` (`64 B`). Because `chunks` sits in page-aligned `mmap` memory, read-mostly `sigs` bytes `[0..64)` sit on an isolated 64-byte coherence line on x86\_64 ($L=64$), separate from write-heavy control bitmasks and hands `[64..128)` (while on ARM64 with $L=128$, the entire 64-slot chunk control plane fits in one single 128-byte line).
2. **Stack-Sharded `statStripe` Telemetry:** In v1, indexing `stats [64]statStripe` by `hash & 63` caused all 8 cores reading the **same** Zipfian hot key to execute atomic `LDADDAL` updates against the **same** `statStripe` cache line—triggering false-sharing/ping-pong across cores even during pure lookups. In `liteLRU` v2, `workerStripe()` hashes the goroutine's 2 KB-aligned stack pointer (`((sp >> 11) * 0x9e3779b97f4a7c15) >> 57`) across `128` cache-line-padded `statStripe` structs (`sizeof(statStripe) == CacheLineSize`, 128B on ARM64 / 64B on x86\_64). Distinct concurrent workers naturally hash to disjoint cache-line stripes even when hammering a single hot key, eliminating per-read counter ping-pong cleanly (validated in Regime A1, where removing `statStripe` padding degrades throughput by **-15.4%**, §9.4).

We still explicitly distinguish cross-worker counter isolation from **intra-chunk bitmask sharing**: all 64 slots inside chunk $k$ intentionally share `sigs` and `(valid, prob, f0, f1, writing, ghost0, ghost1)` to keep `CTZ` victim selection $O(1)$, mitigated by read-before-`Or` in `promoteFreq` and $R=10$ bounded `W_k` retries.

---

## 8. Benchmark Methodology and Artifacts

Standard Go micro-benchmarks use `b.RunParallel`, which distributes `b.N` iterations across goroutines via `pb.Next()`. While `b.RunParallel` amortizes synchronization over per-goroutine iteration batches rather than executing a shared atomic fetch-and-add on every single call, it still adds harness overhead and aggregates only mean `ns/op` without latency percentiles. Running `BenchmarkParallelLRUCache` (`-count=5`) on `liteLRU` v2 (`80% Get / 20% Add`, 1024 capacity) yields medians of **44.04 ns/op** (1 core, range `42.51–44.75`), **26.71 ns/op** (2 cores, range `24.97–34.92`), **16.13 ns/op** (4 cores, range `15.69–19.08`), and **17.88 ns/op** (8 cores, range `17.70–19.82`, reflecting heterogeneous P/E-core scheduling across Apple M2's 4 performance + 4 efficiency cores), with **12 B/op** and **0 allocs/op** reported by `go test` (`Get` executes `0 B/op, 0 allocs/op`, while the 20% `Add` mix averages `< 0.5` heap allocations per mixed op, rounding to `0 allocs/op`).

Our custom evaluation suite (`benchmarks/hardened_eval/main.go`, 10 independent runs reporting median and interquartile range [IQR]) complements `b.RunParallel` by running tight worker loops over pre-allocated key arrays with no shared iteration counter and recording sampled or standalone latency percentiles directly.

---

## 9. Evaluation

**Platform:** Apple M2 (8-core ARM64: 4 performance + 4 efficiency cores, 24 GB RAM, macOS 26.3, Go 1.25.7). Cache capacity 1024 entries unless stated otherwise. All core microbenchmark sections (§§9.1–9.4, §9.6–9.7, §9.11–9.12) reflect **10-run medians (with IQR where noted)** on **`liteLRU` v2** (Bitmask S3-FIFO + single `atomic.Pointer[cacheEntry]` + stack-sharded `statStripe`); §§9.8–9.10 preserve the earlier pre-hardening `vegeta` HTTP snapshot (`f826423`).

### 9.1 Baseline Throughput and Tail Latency

**Workload:** 8 concurrent workers, $1.6 \times 10^6$ total operations, 80% `Get` / 20% `Add`, paths drawn independently at random from a 1000-route uniform-pool mix (70% over hot 200 keys, 30% over 1000 keys; `benchmarks/hardened_eval`). Measured over 10 runs on `liteLRU` v2 against `otter` (`v1.2.4`) and a lock-protected doubly-linked-list `Mutex LRU`:

| Cache Implementation | Median Ops/sec | IQR (Q1–Q3) | Median p50 | Median p99 |
|----------------------|---------------:|-------------:|-----------:|-----------:|
| `liteLRU`            | 51,316,763     | 49.99M–51.91M | 83 ns     | 500 ns     |
| `otter` (`v1.2.4`)   | 16,362,597     | 16.08M–16.65M | 42 ns     | 9.896 µs   |
| `Mutex LRU`          | 7,042,870      | 6.04M–7.17M  | 145 ns     | 25.00 µs   |

*Note: Latency percentiles in this table were sampled at a 1% rate to limit observer effect on RFO traffic while preserving concurrent throughput.*

Across 10 runs, `liteLRU` v2 delivers a **7.29×** median throughput advantage over `Mutex LRU` and **3.14×** over `otter` (`v1.2.4`), while keeping sampled p99 latency at **500 ns** (vs. **9.896 µs** for `otter` and **25.00 µs** for `Mutex LRU`).

### 9.2 Throughput Scaling Under Contention

To measure multi-core scaling under high eviction and cache-miss churn on `liteLRU` v2, we scale a 1.6M operation **Zipfian** workload ($s=1.001$, $N=100{,}000$, capacity 1024, 80% `Get` / 20% `Add`) across 1 to 8 workers (10 runs per core count):

| Cores | Median Ops/sec | IQR (Q1–Q3)   | Scaling Factor |
|------:|---------------:|--------------:|---------------:|
| 1     | 24,268,125     | 23.79M–24.49M | 1.00×          |
| 2     | 39,554,936     | 38.54M–41.30M | 1.63×          |
| 4     | 57,162,224     | 51.65M–70.91M | 2.36×          |
| 8     | 54,391,781     | 53.59M–62.67M | 2.24×          |

Scaling rises to 4 cores (**2.36×**, `57.16M ops/s`) and flattens slightly at 8 cores (`54.39M ops/s`, **2.24×**). Both hardware asymmetry (Apple M2 pairs 4 high-frequency performance cores with 4 slower efficiency cores when concurrency exceeds 4) and hot-chunk atomic CAS traffic ($W_k$, `chk.sigs`, and bitmask updates under heavy Zipf skew) contribute to this sub-linear ceiling. Compared to v1 (`23.34M ops/s` at 8 cores), eliminating seqlock read-side validation and false-sharing on global hit/miss telemetry raises 8-core Zipfian throughput by **2.33×**.

### 9.3 Latency Percentiles (ARM64)

Measured over 10 uncached executions of `TestConcurrentLatency` (8 workers, 1.6M operations, `31.86M ops/s` median throughput, IQR `29.66M–33.26M`):

| Percentile | Raw 10-Run Median | Raw Observed Range |
|:----------:|------------------:|-------------------:|
| p50        | 83 ns             | 83–125 ns          |
| p99        | 500 ns            | 459–583 ns         |
| p99.9      | 1.17 µs           | 917 ns–3.58 µs     |
| Max        | 5.80 ms           | 3.25–17.24 ms      |

*Measurement note:* These raw latencies include per-sample `time.Now()` timing overhead (~40 ns/call on ARM64); under a simplified additive constant subtraction, underlying operation medians correspond approximately to ~43 ns (p50), ~460 ns (p99), and ~1.13 µs (p99.9). The extreme tail (`Max` 3.25–17.24 ms) reflects Go goroutine/OS scheduling preemption on one or more of the eight workers; the test does not isolate scheduler preemption from runtime background activity.

### 9.4 Controlled Ablation Study

To isolate the contribution of SWAR signatures, $L$-byte padding on `statStripe`/slot arrays, and lock-free 64-way set associativity **strictly without allocation-placement confounding**, all three set-associative variants (`Full v2`, `No SWAR (mmap)`, and `No Pad (mmap)`) allocate their `chunks` metadata via off-heap `mmap` and share identical single-pointer `atomic.Pointer[cacheEntry]` slots (`benchmarks/hardened_eval`, 10 runs, no timing sampling, capacity 1024, 8 workers):
- **Regime A0 (Zero-Overflow Pre-Warmed 100% `Get`, 960 keys):** Constructed with `<= 60` keys per chunk across all 16 sets and 100% pre-warmed prior to timing (`0` misses, `0` evictions, `0` drops).
- **Regime A1 (Uniform-Pool 80/20, 1,000 keys):** Empirically audited hash distribution across the 16 chunks shows `minH=52`, `maxH=77`, with **7/16 sets overflowing** ($H_k > 64$, 32 excess keys total), yielding `1,263,831` hits, `16,169` misses (`98.74%` hit rate), and `703` drops—demonstrating that $N_{\text{keys}} = 1{,}000 < 1{,}024$ is *low-eviction* rather than eviction-free.
- **Regime B (Zipfian 80/20, $N=100{,}000$, $s=1.001$):** Continuous misses and active S3-FIFO eviction.
- **Regime C (Zipfian 50/50, $N=100{,}000$, $s=1.001$):** Active eviction paired with heavy write contention.

We also compare against `True MutexLRU` (`sync.Mutex` + doubly-linked list) and `Map+RWMutex*` (Go `map` + `sync.RWMutex` omitting per-read LRU promotion):

| Variant | Regime A0: Zero-Overflow Resident 100% Get | Regime A1: Uniform 80/20 (7/16 sets overflow) | Regime B: Zipf 80/20 Active Evict | Regime C: Zipf 50/50 Write-Heavy |
|---------|-------------------------------------------:|----------------------------------------------:|----------------------------------:|---------------------------------:|
| **Full `liteLRU` v2** | 88.62M (IQR 81.47M–120.10M) | **47.33M** (IQR 35.91M–53.16M) | 42.71M (IQR 35.68M–51.29M) | **30.55M** (IQR 27.19M–32.14M) |
| No SWAR (`mmap`) | 24.00M (**-72.9%**) | 21.04M (**-55.5%**) | 16.31M (**-61.8%**) | 12.25M (**-59.9%**) |
| No Padding (`mmap`) | **97.08M** (**+9.5%**) | 40.05M (**-15.4%**) | **49.75M** (**+16.5%**) | 30.43M (-0.4%) |
| True `MutexLRU` | 8.00M (**-91.0%**) | 6.45M (**-86.4%**) | 5.87M (**-86.3%**) | 5.15M (**-83.1%**) |
| `Map+RWMutex*` (non-LRU) | 13.75M (**-84.5%**) | 22.35M (**-52.8%**) | 13.38M (**-68.7%**) | 8.22M (**-73.1%**) |

Controlling allocation placement (`mmap` vs. `mmap`) on v2 highlights the shift after removing per-slot seqlocks:
1. **SWAR becomes essential across *all four* regimes:** Without 128-byte seqlocks on the read path, SWAR signature pre-filtering is the dominant speedup mechanism across every workload: removing SWAR drops throughput by **-72.9%** in `Regime A0` (`24.00M` vs. `88.62M`), **-55.5%** in `Regime A1`, **-61.8%** in `Regime B`, and **-59.9%** in `Regime C`, because scanning 60–64 `atomic.Pointer[cacheEntry]` pointers triggers dependent cache misses whereas SWAR filters candidate indices in 8 contiguous `uint64` registers (`64 B`).
2. **Padding vs. cache density is workload-dependent:** Because v2 already removes per-slot seqlocks (`0` read-side coherence writes on slot hits), the remaining $L$-byte padding (`statStripe`) trades telemetry write isolation against chunk metadata locality. Removing `statStripe` padding improves cache-resident `100% Get` (`Regime A0`, **+9.5%**) and skewed `Regime B` (**+16.5%**), breaks even under heavy 50/50 churn (`-0.4%`), and hurts uniform hot-pool `80/20` (`Regime A1`, **-15.4%**) where unpadded adjacent counters amplify cross-core bouncing.
3. **Set-associative lock freedom widens the gap over mutexes:** On v2, `True MutexLRU` trails by **-83.1% to -91.0%** across every regime, and even non-evicting `Map+RWMutex*` trails by **-52.8% to -84.5%**.

### 9.5 x86_64 Smoke Test

To verify behavioral consistency across architectures, we evaluated `liteLRU` on an x86_64 Docker container instance. The x86_64 measurement is a behavioral smoke test only; its virtualized environment and different microarchitecture do not support a direct cross-platform performance comparison.

| Architecture | Cores | Ops/sec (x86_64) |
|--------------|------:|-----------------:|
| x86\_64       | 8     | 13,155,469       |

*Note: The x86_64 evaluation was executed within a virtualized Docker Desktop environment. Because hypervisor layers introduce non-deterministic scheduling jitter and distort cache-coherence traffic (such as Read-For-Ownership loops), these metrics are presented strictly as a functional and behavioral smoke test to verify cross-platform correctness of the atomic bitmask and TZCNT mechanics, rather than a bare-metal microarchitectural performance baseline.*

### 9.6 Zipfian Hit-Rate Evaluation (Matched Capacities)

Because `NewLRUCache` rounds requested capacities up to the next power of two (`nextPowerOfTwo`), evaluating arbitrary non-power-of-two sizes (`25k, 50k, 75k, 95k`) against an exact-capacity cache gives `liteLRU` 31%–75% more effective slots (`32,768`, `65,536`, `131,072`). To eliminate that capacity confounding, we compare both caches at **matched power-of-two capacities** (`16,384`, `32,768`, `65,536`, corresponding to 16.4%, 32.8%, and 65.5% of the $N=100{,}000$ working set) with a 20% warmup phase over 10 independent runs (`hardened_eval --only-zipf`):

| Matched Capacity | `liteLRU` ($s=1.001$) | `otter` ($s=1.001$) | `liteLRU` ($s=0.800$) | `otter` ($s=0.800$) |
|------------------|----------------------:|--------------------:|----------------------:|--------------------:|
| 16,384 (16.4%)   | **83.05%**            | 80.67%              | **62.88%**            | 62.83%              |
| 32,768 (32.8%)   | **88.89%**            | 87.38%              | **74.38%**            | 73.83%              |
| 65,536 (65.5%)   | **95.12%**            | 94.70%              | **88.82%**            | 87.09%              |

Replacing v1's 1-bit CLOCK with **Bitmask S3-FIFO + 128-slot Ghost Filter** reverses the eviction-quality deficit observed in Rev 4. Across all six matched-capacity points ($s=1.001$ and $s=0.800$), `liteLRU` v2 now matches or outperforms `otter v1.2.4` [[18]](#ref-otter) (`83.05%` vs. `80.67%` at 16.4% capacity under $s=1.001$, and `88.82%` vs. `87.09%` at 65.5% under $s=0.800$), because separating one-hit scans into `prob` while promoting Ghost-recalled keys directly into frequency-protected `main` preserves skewed working sets without sacrificing per-chunk bitmask parallelism.

### 9.7 Write-Heavy Workloads, Semantic-Equivalence Controls, and Miss Penalty

We omit Ristretto from this benchmark because, as documented in [[18]](#ref-otter), it exhibits hit-rate degradation under extreme concurrent write pressure. Comparing write-heavy caches requires separating three distinct workloads (`benchmarks/hardened_eval --only-write`, 10 runs, 8 workers, Zipfian $s=1.001$):
1. **Direct overwrite contention (`9.7a`, cap `1024`, $N=1{,}000$, 50/50 and 20/80 `Get`/`Add`):** Threads issue explicit `Add`/`Set` operations directly against hot keys. Under `liteLRU`, `4.67%–5.30%` of redundant same-slot overwrites are shed via `W_k` collision drops (non-linearizable best-effort updates), whereas `otter` queues every write internally; discounting dropped writes yields **effective** completed throughput, though semantic work differs when strict update publication is required.
2. **Immutable insert-on-miss maintenance (`9.7b`, cap `1024` with $N=1{,}000, 5{,}000$; cap `16,384` with $N=100{,}000$):** Workers call `Get` and issue `Add`/`Set` **only on a cache miss**, representing memoized routing or immutable-value caches (`0.00%` miss drops for `liteLRU`). Note that `otter`'s asynchronous write buffer allows realized `liveSize` to transiently overshoot nominal capacity (`1,850` vs. `1,024` at $N=5\text{k}$, and `17,224` vs. `16,384` at $N=100\text{k}$).
3. **Explicit Backend Miss-Penalty Control (`9.7c`, cap `16,384`, $N=100{,}000$, $T_{\mathrm{miss}} = 5\,\mu\text{s}$):** Following the application cost model $E[T] \approx h T_{\mathrm{hit}} + (1 - h) T_{\mathrm{miss}}$, each cache miss pays an explicit `5 µs` backend fill penalty prior to calling `Add`/`Set` so both raw throughput and hit rate directly determine end-to-end throughput.

| Workload | `liteLRU` Attempted / Eff. (IQR) | Drops / Hit Rate | `otter` (`v1.2.4`) (IQR) / Hit | Throughput Ratio |
|---|---:|---:|---:|---:|
| `9.7a` Direct Overwrite 50/50 ($N=1\text{k}$) | 33.30M / **32.51M** (29.16M–34.50M) | 4.67% Adds / 99.14% | 6.10M (5.75M–6.22M) | **5.33×** eff. |
| `9.7a` Direct Overwrite 20/80 ($N=1\text{k}$) | 27.76M / **26.57M** (25.44M–27.18M) | 5.30% Adds / 99.20% | 4.04M (3.99M–4.10M) | **6.58×** eff. |
| `9.7b` Insert-on-Miss (cap `1024`, $N=1\text{k}$) | **128.59M** (114.71M–130.09M) | 0.00% miss / 99.19% | 113.74M (108.42M–117.70M) / **99.94%** | **1.13×** (`liteLRU` leads) |
| `9.7b` Insert-on-Miss (cap `1024`, $N=5\text{k}$) | **47.24M** (43.52M–49.85M) | 0.00% miss / 80.76% | 13.11M (12.96M–13.28M) / **85.81%** | **3.60×** (`liteLRU` leads) |
| `9.7b` Insert-on-Miss (cap `16k`, $N=100\text{k}$) | **56.29M** (55.10M–59.32M) | 0.00% miss / **82.20%** | 9.12M (9.02M–9.25M) / 80.17% | **6.17×** (`liteLRU` leads) |
| `9.7c` Miss-Penalty $5\,\mu\text{s}$ (cap `16k`, $N=100\text{k}$) | **4.51M** (4.36M–4.96M) | 0.00% miss / **79.83%** | 3.47M (3.32M–3.59M) / 79.18% | **1.30×** (`liteLRU` leads) |

These semantic-equivalence and miss-penalty controls establish three clear improvements over v1:
- **Winning read-mostly insert-on-miss too (`9.7b`, $N=1\text{k}$):** Removing read-side seqlock RFO traffic flips v1's `0.36×` deficit (`33.64M` vs. `94.27M`) into a **1.13× lead** (`128.59M` vs. `113.74M ops/s`), because read hits perform zero writes to slot state and promote `f0`/`f1` bits only lazily.
- **Large advantage under eviction churn (`9.7b`, $N=5\text{k}$ and $N=100\text{k}$):** At `cap=1024, N=5k`, `liteLRU` leads by **3.60×** (`47.24M` vs. `13.11M`) even while Otter's async buffer transiently holds `liveSize=1,850`; at realistic scale (`cap=16,384, N=100k`), `liteLRU` delivers **6.17×** higher throughput (**56.29M** vs. **9.12M**) *and* a higher hit rate (**82.20%** vs. **80.17%**, despite Otter holding `liveSize=17,224`).
- **Realized end-to-end gain under explicit miss cost (`9.7c`):** When every miss pays a **5 µs** backend penalty, `liteLRU` sustains **4.51M ops/s** vs. `otter`'s **3.47M ops/s** (**1.30×** faster overall, with both `79.83%` vs. `79.18%` hit rate and lower cache hit latency).

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

To characterize `liteLRU` v2's set-associative and Bitmask S3-FIFO behaviors quantitatively (`benchmarks/adversarial_bench.go`, 1024-entry cache, 8 workers), we define per-chunk **set pressure** $P_k = H_k / 64$, where $H_k$ is the active working-set size hashing to chunk $k$:

| Experiment | Configuration | Observed Result |
|---|---|---|
| Single-set contention | 128 keys $\to$ chunk 0 (80/20 Get/Add) | 14,076,060 attempted / 13,975,522 effective ops/s; 3.57% Adds dropped (11,428); 45.90% hit rate |
| Associativity boundary | 64 vs. 65 keys in chunk 0 (1000 cyclic passes) | **100.00%** hit rate at $H_0=64$ ($P_0=1.00$) vs. **96.83%** at $H_0=65$ ($P_0=1.016$) |
| Occupancy (100% Get) | 1% / 50% / 100% fill (pure lookup) | 200.62M / 142.54M / 123.17M ops/s |
| Occupancy (80/20 Mixed) | 1% / 50% / 100% fill (with writes) | 64.40M / 63.89M / 55.93M ops/s (100.00% / 100.00% / 95.06% hit rate) |
| Sequential scan > capacity | 256 hot + 2048 scan keys | **256/256 (100.00%)** hot keys retained (scan-resistant via `prob` + Ghost Filter) |
| Single-key concurrent upsert | 8 workers, 1.6M `Add`s to `/single-hot` | 26.44M attempted / 5.05M effective ops/s; **80.90%** redundant writes shed (1,294,332 drops) |
| Key length scaling | 100% `Get`, 16 / 64 / 256 / 1024-byte keys | 126.31M / 101.49M / 60.90M / 21.99M ops/s |

These experiments demonstrate how Bitmask S3-FIFO + immutable `cacheEntry` transform v1's structural edge cases:
1. **Cyclic Thrash Immunity ($P_k = 1.016$):** Under v1's 1-bit CLOCK, $H_0=65$ keys cycling over a 64-slot chunk collapsed to `0.00%` hits. Under v2's Bitmask S3-FIFO, frequent entries protected in `main` (`f1`/`f0` > 0) stay pinned while `prob` absorbs the overflow key, preserving a **96.83%** hit rate at $H_0=65$.
2. **Complete Scan Resistance:** A sequential 2048-key scan exceeding total cache capacity (`1024`) evicts zero hot entries (**256/256, 100.00%** retained vs. **0/256** in v1), because single-access scan items circulate within the bounded 25% `prob` queue while established items reside in `main`.
3. **High-Occupancy Lookup Stability:** Because `Get` executes zero seqlock retries and zero pointer chases on non-matching slots, read throughput stays above **123M ops/s** even at 100% occupancy (`200.62M / 142.54M / 123.17M` across 1% / 50% / 100% fill), while single-key upsert contention sheds **80.90%** of redundant overwrites without lock convoying.

### 9.12 Off-Heap Metadata vs. On-Heap Slices (GC Isolation)

To evaluate off-heap `mmap` metadata placement cleanly in v2 (where per-slot `slotState` structs have been eliminated, leaving only **`128 KiB`** of `chunk` bitmask/SWAR arrays at `65,536` slots = `2 B/slot`), we run the isolated subprocess suite (`hardened_eval` Section 6, identical single-pointer `atomic.Pointer[cacheEntry]` slots in both variants, `runtime.KeepAlive` held through post-run `runtime.GC()`) across three regimes over equal **2.0-second windows** (8 workers):
1. **`GOGC=100` (Churn):** 80/20 `Get`/`Add` + background `512 B` heap allocation every 8 ops under default Go heap-growth pacing (`nextGC ≈ 2× liveHeap`).
2. **`GOMEMLIMIT=64 MiB` (`GOGC=off`, Churn):** Same 80/20 + `512 B` churn under a fixed 64 MiB soft memory ceiling (`debug.SetGCPercent(-1)` + `debug.SetMemoryLimit(64<<20)`).
3. **`Steady-State` (Zero-Alloc Lookup):** Pre-populated `65,536` slots, 100% `Get` (`0` allocations on hot path), 20 forced `runtime.GC()` cycles over 2 seconds.

| Experiment / Variant | Throughput | NumGC | Total Pause (Mean) | Post-GC `HeapAlloc` (`Inuse`) | Live `HeapObjects` (`Mid` -> `Live`) | `Mallocs` / `TotalAlloc` | `GCCPUFraction` |
|---|---:|---:|---:|---:|---:|---:|---:|
| **`GOGC=100` Off-heap `mmap`** | **29.26M ops/s** | 417 | 44.66 ms (0.107 ms) | **6.51 MB** (9.05 MB) | 191,775 -> 128,352 | 19.02M / 4.29 GB | 0.21368 |
| **`GOGC=100` On-heap slices** | 28.63M ops/s | **399** | **44.34 ms** (0.111 ms) | 6.63 MB (9.28 MB) | 175,338 -> 128,284 | 18.63M / 4.20 GB | **0.20597** |
| **`GOMEMLIMIT=64M` Off-heap** | 34.42M ops/s | **134** | **22.89 ms** (0.171 ms) | **6.50 MB** (8.86 MB) | 331,695 -> 128,227 | 22.45M / 5.06 GB | **0.07611** |
| **`GOMEMLIMIT=64M` On-heap** | **36.96M ops/s** | 145 | 22.95 ms (0.158 ms) | 6.62 MB (7.84 MB) | 333,351 -> 128,216 | 24.08M / 5.43 GB | 0.07971 |
| **`Steady-State` Off-heap** | 94.78M ops/s | 20 | **5.69 ms** (0.284 ms) | **6.47 MB** (7.11 MB) | `Live`: 128,096 | 37 / 0.00 MB | 0.00607 |
| **`Steady-State` On-heap** | **101.55M ops/s** | 20 | 6.25 ms (0.312 ms) | 6.60 MB (7.20 MB) | `Live`: 128,093 | 35 / 0.00 MB | **0.00457** |

Moving from v1 to v2 fundamentally narrows the Go-heap delta:
1. **Why GC gap nearly vanishes in v2 (`128 KiB` vs. `8.1 MiB`):** Because v2 eliminates `128 B` per-slot `slotState` structs (`8.0 MiB` at `65,536` slots), only the `128 KiB` `chunk` array moves off-heap (`HeapAlloc` `6.47 MB` vs. `6.60 MB`, live objects `128,096` vs. `128,093`). Consequently, under `GOGC=100` both placements behave nearly identically (`417` vs. `399` GC cycles, `44.66 ms` vs. `44.34 ms` STW pause, `0.21368` vs. `0.20597` `GCCPUFraction`), eliminating v1's 2.27× `GOGC=100` trigger amplification while still trimming `GOMEMLIMIT=64 MiB` collection frequency by **7.6%** (`134` vs. `145`).
2. **Go Runtime Headroom vs. System RSS:** Importantly, `GOMEMLIMIT` governs **Go's managed runtime heap accounting**, not total OS resident memory (`RSS`). Anonymous `mmap` regions still consume physical RAM pages outside `runtime.MemStats`, so off-heap placement frees *Go GC heap headroom* (relevant under tight `GOMEMLIMIT` quotas or multi-million-slot caches) rather than reducing system-wide physical RAM consumption.

## 10. Discussion and Limitations

**Best-Effort, Non-Linearizable Admission & API Discoverability.** Because `Add` is not globally serialized across slots within a chunk, concurrent cold misses on the same key can insert duplicates in distinct slots, and `Get` always returns the lowest-index match rather than the most recently completed admission. Furthermore, under pathological single-key contention (§9.11), `Add` silently sheds up to 80.90% of redundant overwrites (`drops++`) to bound retry work, and concurrent overwrites yield best-effort visibility until the published `atomic.Pointer[cacheEntry]` becomes visible. Both the Go package documentation (`godoc` on `Add`, `Get`, and `Clear`) and callers must treat `liteLRU` explicitly as a **best-effort, non-linearizable cache** suited for immutable, idempotent, or staleness-tolerant workloads rather than a strictly serializable map.

**Blocking `Clear()` Under Continuous Writes.** Unlike `Get` (bounded probes) and `Add` ($R=10$ bounded claim retries), `Clear()` waits via `runtime.Gosched()` and `CompareAndSwap(0, ^uint64(0))` for a zero `W_k` window per chunk, operating **non-atomically across chunks**. Continuous concurrent writes can starve `Clear()`, so callers needing deterministic teardown latency or global point-in-time emptiness must quiesce writers externally.

**Memory Overhead (16.2× Reduction in Fixed Metadata).** Eliminating 128-byte padded per-slot seqlocks and replacing 4 SoA pointer arrays with a single `atomic.Pointer[cacheEntry]` (`8 B/slot`) plus `2 B/slot` chunk metadata reduces fixed per-slot arrays on Apple Silicon ($L=128$) from `162 B/slot` in v1 to **10 B/slot** (**~10 MB per million slots**, a **16.2×** reduction), plus one immutable `64 B` `cacheEntry` struct per populated key and caller key/parameter storage (**~74 MB per million active slots** total metadata+header footprint, vs. `~120 MB` for `otter`). Stack-sharded telemetry (`[128]statStripe`) adds a fixed **`16 KiB`** per `LRUCache` instance regardless of capacity.

**Set-Associative Capacity Boundaries vs. Global Policies.** Each chunk operates an independent 64-way set-associative Bitmask S3-FIFO policy (`25% prob / 75% main` + 128-slot `ghost` filter). As demonstrated in §9.6 and §9.11, Bitmask S3-FIFO makes `liteLRU` 100% scan-resistant (`256/256` hot keys retained) and avoids 65-key cyclic collapse (`96.83%` vs. `0.00%` in v1), matching or beating `otter` on Zipfian hit rates at matched capacities (`83.05%–95.12%`). Nevertheless, extreme hash concentration into a single chunk ($H_k \gg 64$, e.g. 128 hot keys in one set) will still evict cold keys locally even if other chunks sit under-utilized.

**Fixed Capacity.** The cache does not resize and rounds requested capacities up to powers of two (`nextPowerOfTwo`, minimum 64). While this eliminates steady-state resizing allocations, callers must account for power-of-two capacity rounding when provisioning memory.

**Extreme Contention Load-Shedding & Scoped Bounds.** To bound ownership-claim work, `liteLRU` limits CAS retries in `findVictim` and per-matched-slot `W_k` acquisition to $R=10$ attempts, and sheds redundant updates when a slot's `W_k` bit is already held. Post-claim signature-word CAS (`chk.sigs[sigWordIdx]`) lies outside $R$ (Theorem 2). Under pathological single-key upsert contention (§9.11), `liteLRU` sheds **80.90%** of redundant writes to prevent unbounded claim spinning.

**Zero-Allocation Scope.** `Get` guarantees **0 heap allocations** (verified via `testing.AllocsPerRun`) on cache misses and on cache hits whenever the caller supplies a `dst` slice with `cap(dst) >= len(params)` (including `nil` when `len(params) == 0`). By contrast, `Add` allocates one immutable `64 B` `cacheEntry` (plus fresh `[]Param` backing storage when `len(params) > 0`) on miss/overwrite publication so readers need zero locks or sequence retries.

---

## 11. Conclusion

We have presented the architecture and evaluation of `liteLRU` v2, deriving each mechanism from cache-coherence and runtime constraints: a hybrid memory architecture packing `chunk` SWAR signatures and Bitmask S3-FIFO state into off-heap `mmap` blocks (`2 B/slot`) while tracking single-pointer immutable `atomic.Pointer[cacheEntry]` slots (`8 B/slot`) safely under Go's GC [[9,15]](#ref-gogc); stack-sharded $L$-byte-padded telemetry (`[128]statStripe`) to remove global atomic bounce [[1,8]](#ref-mesi); contiguous 64-way `chunk.sigs` SWAR pre-filtering [[10,11]](#ref-tzcnt) cutting read probes to a single pointer dereference on match; and Bitmask S3-FIFO with a 128-slot Ghost Filter per chunk eliminating CLOCK's cyclic-thrash and scan vulnerabilities. Across 10-run controlled evaluations (§§9.1–9.7, §§9.11–9.12), `liteLRU` v2 outperforms `otter v1.2.4` across both read-dominated (`128.59M` vs. `113.74M ops/s`, **1.13×**) and evicting (`47.24M–56.29M` vs. `9.12M–13.11M ops/s`, **3.60×–6.17×**) workloads—including under an explicit `5 µs` backend miss penalty (**1.30×**)—while achieving **100.00%** scan retention, matching or exceeding `otter` on Zipfian hit rates, and cutting tail p99 latency on the uniform baseline to **500 ns** (vs. **9.896 µs**).

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
* **macOS Version**: macOS 26.3
* **Machine RAM**: 24GB (`hw.memsize = 25769803776`)
* **v2 Evaluation Measurements**: §§8 (`BenchmarkParallelLRUCache`, 5 runs), 9.1, 9.2, 9.4, 9.6, 9.7 (10 runs each via `benchmarks/hardened_eval`, reporting median and IQR), §9.3 (`TestConcurrentLatency`, 10 uncached runs), and §§9.11–9.12 (`adversarial_bench.go`, `hardened_eval` Section 6) were all measured on `liteLRU` v2 (Bitmask S3-FIFO + immutable `cacheEntry` + stack-sharded `statStripe`).
* **Historical HTTP Snapshot**: §§9.8–9.10 preserve the earlier pre-hardening `vegeta` HTTP snapshot from commit `f826423b006c4df62cb1fc1ddfcf76eb2e9b0682` (with `server.go` requesting `capacity=75000`, which rounded to `131,072` slots for `liteLRU` vs. `75,000` for Otter; note `benchmarks/ablation/` also preserves an earlier hash-map ablation harness).
* **Comparator**: `github.com/maypok86/otter v1.2.4`
* **Environment**: `GOMAXPROCS=8`
* **Commands**:
  - Race & correctness verification: `go vet ./... && go test -v -race -count=1 ./...`
  - Latency & parallel Go harnesses: `go test -v -count=10 -run=TestConcurrentLatency .` and `go test -run=^$ -bench=BenchmarkParallelLRUCache -benchmem -cpu=1,2,4,8 -count=5 .`
  - Unified evaluation suite (`cd benchmarks`): `go run ./hardened_eval` (or scoped runs `--only-baseline`, `--only-ablation`, `--only-scale`, `--only-write`, `--only-zipf`, `--only-gc`), `go run adversarial_bench.go`
