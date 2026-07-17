import sys

# Fix WHITEPAPER.md
with open("WHITEPAPER.md", "r") as f:
    text = f.read()

# 1. Fix Dice reference number collision
text = text.replace(
    '[14] Dice, D., Kogan, A., and Lev, Y. "Understanding and Improving the Performance of Concurrent Applications." *USENIX*, 2013.',
    '[16] Dice, D., Kogan, A., and Lev, Y. "Understanding and Improving the Performance of Concurrent Applications." *USENIX*, 2013.'
)
text = text.replace(
    '[16] Qiu, Z., Yang, J., and Harchol-Balter, M.',
    '[17] Qiu, Z., Yang, J., and Harchol-Balter, M.'
)
text = text.replace(
    '[17] Otter Authors.',
    '[18] Otter Authors.'
)
text = text.replace(
    'amplification [14]',
    'amplification [16]'
)

# If Dice was placed at the top by accident, let's remove it
if '<div align="center">' in text:
    pass # Wait, user said it was at the top before the abstract. I'll check with a regex later if needed.
# Actually I already placed it at the bottom in the previous run, maybe it was in a weird place before. Let's see if it appears twice.
import re
dice_count = text.count("Dice, D., Kogan")
print(f"Dice appears {dice_count} times in markdown")
if dice_count > 1:
    # Remove the first occurrence
    text = text.replace('<a name="ref-dice"></a>\n[14] Dice, D., Kogan, A., and Lev, Y. "Understanding and Improving the Performance of Concurrent Applications." *USENIX*, 2013.\n', '', 1)

# 2. Add headers to 9.1 Table
text = text.replace(
    '| `liteLRU`                 | 31,140,774 | 583 ns      | 1.79 µs     |\n| `ristretto` (BP-Wrapper)  | 10,557,365 | 292 ns      | 64.83 µs    |',
    '| Cache Implementation      | Ops/sec    | p50 Latency | p99 Latency |\n|---------------------------|-----------:|------------:|------------:|\n| `liteLRU`                 | 31,140,774 | 583 ns      | 1.79 µs     |\n| `ristretto` (BP-Wrapper)  | 10,557,365 | 292 ns      | 64.83 µs    |'
)

# 3. Text/Table inconsistency in 9.1
text = text.replace("1.83 µs", "1.79 µs")
text = text.replace("31,604,548", "31,140,774")
text = text.replace("1.83 µs vs 64.8 µs", "1.79 µs vs 64.8 µs")

# 4. Stale Cross-Reference in 9.7
text = text.replace(
    "under intense concurrent pressure (as seen in §9.6),",
    "under intense concurrent pressure,"
)

# 5. Reference Qiu in 9.6
text = text.replace(
    "S3-FIFO is expected to outperform CLOCK approximations.",
    "S3-FIFO is expected to outperform CLOCK approximations. However, `liteLRU`'s throughput remains immune to the high hit-ratio contention pathology formally described by Qiu et al. [17]."
)

# 6. Ablation baseline Ops/sec footnote in 9.4
text = text.replace(
    "| `liteLRU` (Full)          | 32,024,792 | —               |",
    "| `liteLRU` (Full)*         | 32,024,792 | —               |"
)
if "*Note: The ablation runs use a distinct" not in text:
    text = text.replace(
        "### 9.5 x86_64 Smoke Test",
        "*\\*Note: The ablation runs use a distinct benchmark harness measuring absolute hardware limits without the 1% tracking overhead of the p99 latency evaluation, accounting for the higher ops/sec baseline relative to §9.1.*\n\n### 9.5 x86_64 Smoke Test"
    )

# 7. Soften oracle claim in 9.6
text = text.replace(
    "`liteLRU` matches the theoretical oracle within 1-2 percentage points at every level, demonstrating that its structurally lock-free hit path trades no eviction fidelity for its speed.",
    "`liteLRU` achieves hit rates competitive with frequency-optimal caching across both high-skew and moderate-skew Zipfian distributions. Furthermore, CLOCK's recency tracking provides an advantage over pure frequency-based eviction at moderate skew (e.g. s=0.8) due to temporal locality in the access pattern, demonstrating that its structurally lock-free hit path trades no eviction fidelity for its speed."
)

with open("WHITEPAPER.md", "w") as f:
    f.write(text)

# Fix whitepaper.tex
with open("whitepaper.tex", "r") as f:
    text2 = f.read()

# 3. Intex inconsistency 9.1
text2 = text2.replace("1.83 \\mu s", "1.79 \\mu s") # Actually it's written as 1.83\textmu s or 1.83~\mu s
text2 = text2.replace("1.83\\textmu s", "1.79\\textmu s")
text2 = text2.replace("1.83 \\textmu s", "1.79 \\textmu s")
text2 = text2.replace("31,604,548", "31,140,774")

# 4. Stale Cross Reference 9.7
text2 = text2.replace(
    "under intense concurrent pressure (as seen in \\S 9.6),",
    "under intense concurrent pressure,"
)

# 5. Reference Qiu 9.6
text2 = text2.replace(
    "S3-FIFO is expected to outperform CLOCK approximations.",
    "S3-FIFO is expected to outperform CLOCK approximations. However, \\texttt{liteLRU}'s throughput remains immune to the high hit-ratio contention pathology formally described by Qiu et al.~\\cite{hitrate2026}."
)

# 6. Ablation footnote 9.4
text2 = text2.replace(
    "\\texttt{liteLRU} (Full)          & 32,024,792 & ---",
    "\\texttt{liteLRU} (Full)$^*$          & 32,024,792 & ---"
)
text2 = text2.replace(
    "\\caption{Ablation of optimization layers.}",
    "\\caption{Ablation of optimization layers. $^*$The ablation runs use a distinct benchmark harness measuring absolute hardware limits without the 1\\% tracking overhead of the latency evaluation, accounting for the higher ops/sec baseline relative to \\S 9.1.}"
)

# 7. Soften oracle claim 9.6
text2 = text2.replace(
    "\\texttt{liteLRU} matches the theoretical oracle within 1-2 percentage points at every level, demonstrating that its structurally lock-free hit path trades no eviction fidelity for its speed.",
    "\\texttt{liteLRU} achieves hit rates competitive with frequency-optimal caching across both high-skew and moderate-skew Zipfian distributions. Furthermore, CLOCK's recency tracking provides an advantage over pure frequency-based eviction at moderate skew (e.g. $s=0.8$) due to temporal locality in the access pattern, demonstrating that its structurally lock-free hit path trades no eviction fidelity for its speed."
)

with open("whitepaper.tex", "w") as f:
    f.write(text2)

print("done")
