import sys
import re

with open("WHITEPAPER.md", "r") as f:
    text = f.read()

# Fix SIEVE citation
text = text.replace("SIEVE [18]", "SIEVE [6]")

# Replace the Zipf table in markdown
old_md_table = """| Cache Capacity | `liteLRU` Hit Rate | `otter` v2 Hit Rate |
|----------------|--------------------|---------------------|
| 25% (25,000)   | **86.62%**         | 84.61%              |
| 50% (50,000)   | **94.48%**         | 91.13%              |
| 75% (75,000)   | **97.59%**         | 95.98%              |
| 95% (95,000)   | **97.59%**         | **97.59%**          |"""

new_md_table = """| Capacity | `liteLRU` (s=1.001) | `otter` (s=1.001) | `liteLRU` (s=0.8) | `otter` (s=0.8) |
|----------|---------------------|-------------------|-------------------|-----------------|
| 25%      | **86.62%**          | 84.61%            | **69.69%**        | 69.54%          |
| 50%      | **94.48%**          | 91.13%            | **87.27%**        | 80.69%          |
| 75%      | **97.59%**          | 95.98%            | **98.74%**        | 90.50%          |
| 95%      | **97.59%**          | **97.59%**        | **98.74%**        | 98.01%          |"""

text = text.replace(old_md_table, new_md_table)

with open("WHITEPAPER.md", "w") as f:
    f.write(text)

with open("whitepaper.tex", "r") as f:
    text2 = f.read()

text2 = text2.replace("SIEVE~\\cite{sieve2024}", "SIEVE~\\cite{sieve2024}")

old_tex_table = """\\begin{table}[h]
\\centering
\\begin{tabular}{@{}rll@{}}
\\toprule
\\textbf{Cache Capacity} & \\textbf{\\texttt{liteLRU} Hit Rate} & \\textbf{\\texttt{otter} v2 Hit Rate} \\\\ \\midrule
25\\% (25,000)            & \\textbf{86.62\\%}                 & 84.61\\%                           \\\\
50\\% (50,000)            & \\textbf{94.48\\%}                 & 91.13\\%                           \\\\
75\\% (75,000)            & \\textbf{97.59\\%}                 & 95.98\\%                           \\\\
95\\% (95,000)            & \\textbf{97.59\\%}                 & \\textbf{97.59\\%}                 \\\\ \\bottomrule
\\end{tabular}
\\caption{Hit rates under Zipfian distribution.}
\\end{table}"""

new_tex_table = """\\begin{table}[h]
\\centering
\\begin{tabular}{@{}rllll@{}}
\\toprule
\\textbf{Capacity} & \\textbf{\\texttt{liteLRU} (s=1.0)} & \\textbf{\\texttt{otter} (s=1.0)} & \\textbf{\\texttt{liteLRU} (s=0.8)} & \\textbf{\\texttt{otter} (s=0.8)} \\\\ \\midrule
25\\%      & \\textbf{86.62\\%} & 84.61\\% & \\textbf{69.69\\%} & 69.54\\% \\\\
50\\%      & \\textbf{94.48\\%} & 91.13\\% & \\textbf{87.27\\%} & 80.69\\% \\\\
75\\%      & \\textbf{97.59\\%} & 95.98\\% & \\textbf{98.74\\%} & 90.50\\% \\\\
95\\%      & \\textbf{97.59\\%} & \\textbf{97.59\\%} & \\textbf{98.74\\%} & 98.01\\% \\\\ \\bottomrule
\\end{tabular}
\\caption{Hit rates under Zipfian distributions with varying skew (s=1.001 and s=0.8).}
\\end{table}"""

text2 = text2.replace(old_tex_table, new_tex_table)

with open("whitepaper.tex", "w") as f:
    f.write(text2)

print("done")
