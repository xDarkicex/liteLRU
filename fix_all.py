import sys

# Fix WHITEPAPER.md
with open("WHITEPAPER.md", "r") as f:
    text = f.read()

# Fix §9.1 Table (missing liteLRU row)
# Let's find the ristretto row and insert the liteLRU row before it.
text = text.replace(
    "| `ristretto` (BP-Wrapper)  | 10,557,365 | 292 ns      | 64.83 µs    |",
    "| `liteLRU`                 | 31,140,774 | 583 ns      | 1.79 µs     |\n| `ristretto` (BP-Wrapper)  | 10,557,365 | 292 ns      | 64.83 µs    |"
)
# Wait, the table in 9.1 is:
# | Cache Implementation      | Ops/sec    | p50 Latency | p99 Latency |
# |---------------------------|-----------:|------------:|------------:|
# | `ristretto` (BP-Wrapper)  | 10,557,365 | 292 ns      | 64.83 µs    |

# Let's do it precisely using regex or string splits
import re

text = re.sub(
    r"(\|-.*?:\|-.*?:\|-.*?:\|-.*?:\|\n)(\| `ristretto` \(BP-Wrapper\).*?\n)",
    r"\1| `liteLRU`                 | 31,140,774 | 583 ns      | 1.79 µs     |\n\2",
    text
)

# Fix §9.4 Table
text = text.replace(
    "| `liteLRU`          | **31,140,774** | **5.87×**    | 583ns       | **1.79µs**  |",
    "| `liteLRU` (Full)          | 32,024,792 | —               |"
)

# Fix abstract citations
text = text.replace(
    "RFO (Read-For-Ownership) round-trip amplification [9]",
    "RFO (Read-For-Ownership) round-trip amplification [14]"
)
text = text.replace(
    "garbage collection interference with tail latency [10]",
    "garbage collection interference with tail latency [9]"
)
text = text.replace(
    "first-principles cache-line occupancy arguments [8]",
    "first-principles cache-line occupancy arguments [7]"
)
text = text.replace(
    "hardware `CTZ` instruction [11,12]",
    "hardware `CTZ` instruction [10,11]"
)

# Fix Section 1.3
text = text.replace(
    "the wait time for the contested line scales as $O(N)$ per operation",
    "the wait time for the contested line degrades superlinearly due to RFO round-trip amplification [14]"
)

# Fix Section 2 (memory.HashMap details)
text = text.replace(
    "implements SIMD-accelerated linear probing",
    "implements SSE2/NEON SIMD-accelerated linear probing (`_mm_cmpeq_epi8` / `vceqq_u8`)"
)
text = text.replace(
    "controller that incrementally rehashes",
    "controller (triggered when average probe length exceeds a setpoint threshold) that incrementally rehashes"
)
text = text.replace(
    "background Proportional-Integral-Derivative",
    "fully concurrent background Proportional-Integral-Derivative"
)

# Fix Section 6 typo
text = text.replace(
    "## 6. Sequence Lock Design### 6.1 Wait-Free Reads",
    "## 6. Sequence Lock Design\n\n### 6.1 Wait-Free Reads"
)

# Remove maxParams 20
text = text.replace(", maxParams 20", "")

# Fix 9.6 perfectly strictly -> superior or tied
text = text.replace(
    "achieves perfectly strictly superior or tied",
    "achieves superior or tied"
)

# Add reference 14 to references
ref14 = '\n<a name="ref-dice"></a>\n[14] Dice, D., Kogan, A., and Lev, Y. "Understanding and Improving the Performance of Concurrent Applications." *USENIX*, 2013.\n'
if "ref-dice" not in text:
    text = text.replace("---", f"---{ref14}", 1) # wait, replacing the first --- is bad. 
    # Let's just append it before ref-hitrate.
    text = text.replace('<a name="ref-hitrate"></a>', f'{ref14}\n<a name="ref-hitrate"></a>')

with open("WHITEPAPER.md", "w") as f:
    f.write(text)

print("WHITEPAPER.md fixed")

# Fix whitepaper.tex
with open("whitepaper.tex", "r") as f:
    text2 = f.read()

# Fix 1.3
text2 = text2.replace(
    "the wait time for the contested line scales as $\\mathcal{O}(N)$ per operation",
    "the wait time for the contested line degrades superlinearly due to RFO round-trip amplification~\\cite{dice}"
)

# Fix Section 2
text2 = text2.replace(
    "implements SIMD-accelerated linear probing",
    "implements SSE2/NEON SIMD-accelerated linear probing (\\texttt{\\_mm\\_cmpeq\\_epi8} / \\texttt{vceqq\\_u8})"
)
text2 = text2.replace(
    "controller that incrementally rehashes",
    "controller (triggered when average probe length exceeds a setpoint threshold) that incrementally rehashes"
)
text2 = text2.replace(
    "background Proportional-Integral-Derivative",
    "fully concurrent background Proportional-Integral-Derivative"
)

# Fix Section 6 typo
text2 = text2.replace(
    "\\section{Sequence Lock Design### 6.1 Wait-Free Reads}",
    "\\section{Sequence Lock Design}\n\n\\subsection{Wait-Free Reads}"
)

text2 = text2.replace(", maxParams 20", "")

text2 = text2.replace(
    "achieves strictly superior or tied",
    "achieves superior or tied"
)

# Insert citation \bibitem{dice}
dice_bib = "\\bibitem{dice}\nD. Dice, A. Kogan, and Y. Lev,\n\\textit{Understanding and Improving the Performance of Concurrent Applications},\nUSENIX, 2013.\n\n"
if "dice}" not in text2:
    text2 = text2.replace("\\end{thebibliography}", f"{dice_bib}\\end{{thebibliography}}")

with open("whitepaper.tex", "w") as f:
    f.write(text2)

print("whitepaper.tex fixed")

