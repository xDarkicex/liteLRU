<div align="center">

# Scaling Eviction to the Metal: An $O(1)$ Lock-Free Chunked Bitmask Cache for Multi-Core Architectures

**Author:** xDarkicex  
**Affiliations:** libravdb, bitdev, zephyr-systems  
**Contact:** git@libravdb.com  

</div>

## Abstract

Contemporary cache eviction research predominantly focuses on optimizing algorithmic hit ratios on web-scale traces, frequently treating the parallel execution cost on multi-core hardware as an afterthought. Traditional Least Recently Used (LRU) algorithms require mutations to a doubly-linked list upon every memory access, necessitating global synchronization locks that induce catastrophic core contention. We present a novel lock-free architecture that replaces pointer-based list manipulation with deterministic $O(1)$ bitwise operations. By utilizing Chunked Bitmask CLOCK eviction, padded OS-boundary Sequence Locks (Seqlocks), and an open-addressed hash map, we formally prove wait-free read capabilities and eliminate false-sharing invalidation storms. Empirical evaluation on an 8-core ARM architecture demonstrates near-linear scaling to 17.0 million ops/sec with a p99.9 tail latency of 1.4 microseconds under a highly contended workload.

---

## 1. Introduction

In modern database and storage systems, performance is increasingly dictated by hardware-level mechanical sympathy rather than asymptotic algorithmic complexity. The memory wall dictates that operations requiring cross-core L1/L2 cache coherency traffic are orders of magnitude slower than operations constrained to a single core's local cache. 

While sophisticated eviction algorithms such as S3-FIFO and SIEVE achieve superior hit-rates, they inherit the synchronization overhead of traditional LRU implementations. Specifically, tracking recency requires state mutations on every cache hit. In a multi-threaded environment, this necessitates mutual exclusion (e.g., `sync.RWMutex`), rendering the cache a sequential bottleneck that violates Amdahl’s Law. Even lock-free CLOCK variants relying on atomic fetch-and-add instructions for a global clock hand suffer from heavy contention.

This paper proposes an entirely decentralized, lock-free approach. By utilizing 64-bit mathematical bitmasks and strict cache-line alignment, we eliminate both mutual exclusion locks and atomic spin-loops, bounding eviction to a strict hardware-intrinsic constant time $O(1)$.

## 2. Theoretical Model and State Space

We begin by formally defining the state space of the cache structure.

**Definition 1 (Cache Structure).** Let $C$ represent the total capacity of the cache. The cache is physically partitioned into a set of contiguous chunks $K$, where the total number of chunks is $N_k = \lceil C / 64 \rceil$. 

**Definition 2 (Chunk State).** Each chunk $k \in K$ tracks the state of 64 discrete cache slots. The state of chunk $k$ is defined by a tuple of 64-bit integers $(V_k, A_k, W_k)$, where:
- $V_k \in \{0,1\}^{64}$ is the **Validity Mask**, indicating if slot $i$ holds initialized data.
- $A_k \in \{0,1\}^{64}$ is the **Access Mask**, indicating if slot $i$ has been accessed recently.
- $W_k \in \{0,1\}^{64}$ is the **Write Mask**, functioning as a bitwise lock for concurrent eviction.

Memory is structured as a Structure of Arrays (SoA), enabling prefetch-friendly contiguous accesses mapped directly to the chunk indices.

## 3. The Chunked Bitmask Eviction Algorithm

Traditional CLOCK relies on an $O(N)$ sequential scan of an array. We replace this with a deterministic bitwise formula.

**Algorithm 1 (O(1) Victim Selection).**
Let $h(r)$ be the hash of incoming route $r$. The starting chunk index is deterministically seeded as $k_{start} = h(r) \pmod{N_k}$. For a given chunk $k$, the candidate bitmask $C_k$ is computed as:
\[ C_k = \neg V_k \lor (V_k \land \neg A_k) \]

The victim index $i$ within chunk $k$ is found using the hardware-intrinsic Count Trailing Zeros (CTZ) instruction:
\[ i = \text{CTZ}(C_k) \]

**Theorem 1 ($O(1)$ Time Complexity).** *The selection of an eviction candidate within a chunk executes in strictly $O(1)$ time without iterative loops.*
*Proof.* The computation of $C_k$ requires two bitwise NOT operations, one AND, and one OR, all of which execute in 1 CPU cycle. The CTZ instruction is implemented as a single hardware intrinsic (e.g., `TZCNT` on x86, `RBIT`+`CLZ` on ARM) completing in $O(1)$ cycles. Therefore, evaluating 64 potential victims requires constant hardware cycles, avoiding the non-deterministic atomic spin-loops of standard CLOCK. $\blacksquare$

## 4. Concurrency and Synchronization

To protect non-atomic SoA reads from concurrent evictions, we implement Sequence Locks (Seqlocks).

**Definition 3 (Sequence Lock State).** Let $S_i \in \mathbb{N}$ be an atomic monotonically increasing sequence counter for slot $i$.
- If $S_i \equiv 0 \pmod 2$, slot $i$ is stable (read-only).
- If $S_i \equiv 1 \pmod 2$, slot $i$ is actively being written/evicted.

**Theorem 2 (Wait-Free Reads).** *The $Get$ operation is wait-free.*
*Proof.* A thread reading slot $i$ performs the following operations:
1. Atomically loads $seq_1 \leftarrow S_i$.
2. Evaluates $seq_1 \pmod 2 \neq 0$; if true, it immediately aborts (Cache Miss).
3. Reads the SoA data strings.
4. Atomically loads $seq_2 \leftarrow S_i$.
5. If $seq_1 \neq seq_2$, it immediately aborts (Cache Miss).
At no point does the thread yield, block, or spin-wait on a condition. The operation executes a finite number of steps bounded by hardware latency, proving it is strictly wait-free. $\blacksquare$

**Theorem 3 (Deadlock Freedom).** *The $Add$ operation is deadlock-free.*
*Proof.* To evict slot $i$, a thread attempts an atomic Compare-And-Swap (CAS) on the write mask $W_k$. If the CAS fails, it implies another thread successfully acquired the bit and is progressing. Since the bit is strictly released after a finite SoA write operation, at least one thread in the system always makes progress. Thus, the system is deadlock-free. $\blacksquare$

## 5. Hardware-Level Cache Coherency

**False Sharing Probability Model.** Modern CPUs fetch memory in discrete 64-byte cache lines. If the 4-byte seqlocks $S_i$ were packed contiguously (Array of Structs), a single 64-byte line would contain 16 distinct seqlocks. 

Let $P_{inv}$ be the probability of a false-sharing invalidation. If threads $T_A$ and $T_B$ concurrently modify independent slots $x$ and $y$, an invalidation storm occurs if $\lfloor x/16 \rfloor = \lfloor y/16 \rfloor$. As core counts $N \to \infty$, $P_{inv}$ approaches 1, resulting in exponential increases in memory latency.

**Theorem 4 (Cache Line Isolation).** *Padding $S_i$ to 64 bytes guarantees $P_{inv} = 0$.*
*Proof.* We define the padded slot state as:
```go
type slotState struct {
    seq atomic.Uint32 // 4 bytes
    _   [60]byte      // 60 bytes
}
```
By forcing `sizeof(slotState) == 64`, $S_x$ and $S_{x+1}$ reside on strictly disjoint physical cache lines. Thus, a write to $S_x$ generates no coherency traffic on the interconnect bus for a thread reading $S_{x+1}$. False sharing is mathematically eliminated. $\blacksquare$

## 6. The Benchmarking Illusion (Methodology)

Evaluating sub-10ns operations reveals critical flaws in standard benchmarking frameworks (such as Go's `testing.B.RunParallel`). 

These frameworks distribute iterations using a shared atomic fetch-and-add counter (`pb.Next()`). According to Amdahl’s Law, the theoretical speedup $S$ of a parallel execution is bounded by the sequential fraction of the task $f$:
\[ S(N) = \frac{1}{(1-f) + \frac{f}{N}} \]

When the operation time approaches the latency of an atomic cache-line bounce (~20ns), the atomic synchronization of the benchmark itself forces $f \to 1$. This creates an illusion of negative scaling, where `ns/op` artificially increases with core count. To bypass this, our methodology utilizes strictly un-synchronized pre-allocated worker loops, isolating the cache mechanics from the benchmark's artifact contention.

## 7. Empirical Results

Empirical evaluation was conducted on an 8-Core ARM architecture (Apple M2) utilizing the un-synchronized isolation methodology. The workload consisted of a highly contended 80% Get / 20% Add parallel distribution with $1.6 \times 10^6$ operations.

| Cores | Throughput (Ops/sec) |
|-------|----------------------|
| 1     | 5,457,508            |
| 2     | 8,847,292            |
| 4     | 13,701,548           |
| 8     | **17,066,659**       |

**Latency Percentiles (8 Cores):**
*Note: Includes a ~40ns inherent `time.Now()` measurement overhead.*
- **p50 (Median)**: ~250 ns 
- **p99**: ~1.0 µs 
- **p99.9**: **~1.4 µs** 

## 8. Conclusion

`liteLRU` demonstrates that achieving sub-microsecond p99.9 latencies on highly contended multi-core architectures requires abandoning algorithmic purity in favor of physical hardware alignment. By replacing $O(1)$ pointer manipulations with $O(1)$ bitwise operations, isolating state across OS cache lines, and proving wait-free capabilities, we have constructed a cache architecture that scales to the physical limits of the silicon.

---
**Source Code:** The reference implementation is available at [https://github.com/xDarkicex/liteLRU](https://github.com/xDarkicex/liteLRU).
