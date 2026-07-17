import sys
import re

# Fix WHITEPAPER.md
with open("WHITEPAPER.md", "r") as f:
    text = f.read()

# 1. Dice [16] Duplicated at Top of Paper
# The text likely looks like:
# <div align="center">
# ...
# <a name="ref-dice"></a>
# [16] Dice, D., Kogan, A., and Lev, Y. "Understanding and Improving the Performance of Concurrent Applications." *USENIX*, 2013.
# ...
# </div>
# I'll just use regex to remove it if it occurs before the Abstract.
first_dice = text.find("Dice, D., Kogan")
abstract_idx = text.find("## Abstract")
if first_dice != -1 and first_dice < abstract_idx:
    # Find the <a name="ref-dice"></a> line before it
    dice_start = text.rfind("<a name=\"ref-dice\"></a>", 0, first_dice)
    if dice_start != -1:
        # Find the end of the line containing "2013."
        dice_end = text.find("2013.", first_dice) + 5
        text = text[:dice_start] + text[dice_end:]

# 2. Otter [18] Uncited in Body
text = text.replace(
    "we benchmarked `liteLRU` against `otter` v2 using",
    "we benchmarked `liteLRU` against `otter` [18] v2 using"
)

# 3. Ristretto Claim Needs a Source
text = text.replace(
    "ristretto's hit-rate collapses under intense concurrent pressure,",
    "ristretto's hit-rate collapses under intense concurrent pressure (as documented by the Otter authors [18]),"
)

# 4 & 5. Caveat Mismatch & "Pure Frequency-Based Eviction" Inaccurate
text = text.replace(
    "It should be noted that this hit-rate parity is characteristic of heavily-skewed ($s=1.001$) heavy-tailed distributions;",
    "It should be noted that this hit-rate advantage holds across both high-skew ($s=1.001$) and moderate-skew ($s=0.8$) distributions;"
)
text = text.replace(
    "on scan-resistant or low-skew workloads, S3-FIFO is expected to outperform CLOCK approximations.",
    "on strictly scan-resistant workloads (e.g., sequential looping), S3-FIFO is expected to outperform CLOCK approximations."
)
text = text.replace(
    "pure frequency-based eviction at moderate skew",
    "S3-FIFO's frequency-aware admission at moderate skew"
)

# 6. "Formally Described" -> "Described"
text = text.replace(
    "formally described by Qiu et al.",
    "described by Qiu et al."
)

with open("WHITEPAPER.md", "w") as f:
    f.write(text)

# Fix whitepaper.tex
with open("whitepaper.tex", "r") as f:
    text2 = f.read()

# 2. Otter Uncited
text2 = text2.replace(
    "we benchmarked \\texttt{liteLRU} against \\texttt{otter} v2 using",
    "we benchmarked \\texttt{liteLRU} against \\texttt{otter}~\\cite{otter} v2 using"
)

# 3. Ristretto Claim Source
text2 = text2.replace(
    "\\texttt{ristretto}'s hit-rate collapses under intense concurrent pressure,",
    "\\texttt{ristretto}'s hit-rate collapses under intense concurrent pressure (as documented by the Otter authors~\\cite{otter}),"
)

# 4 & 5. Caveat & Frequency Fixes
text2 = text2.replace(
    "It should be noted that this hit-rate parity is characteristic of heavily-skewed ($s=1.001$) heavy-tailed distributions;",
    "It should be noted that this hit-rate advantage holds across both high-skew ($s=1.001$) and moderate-skew ($s=0.8$) distributions;"
)
text2 = text2.replace(
    "on scan-resistant or low-skew workloads, S3-FIFO is expected to outperform CLOCK approximations.",
    "on strictly scan-resistant workloads (e.g., sequential looping), S3-FIFO is expected to outperform CLOCK approximations."
)
text2 = text2.replace(
    "pure frequency-based eviction at moderate skew",
    "S3-FIFO's frequency-aware admission at moderate skew"
)

# 6. Formally Described
text2 = text2.replace(
    "formally described by Qiu et al.",
    "described by Qiu et al."
)

with open("whitepaper.tex", "w") as f:
    f.write(text2)

print("done")
