import sys

with open("whitepaper.tex", "r") as f:
    text2 = f.read()

text2 = text2.replace(
    "\\caption{Ablation study throughput comparisons.}",
    "\\caption{Ablation study throughput comparisons. $^*$The ablation runs use a distinct benchmark harness measuring absolute hardware limits without the 1\\% tracking overhead of the latency evaluation, accounting for the higher ops/sec baseline relative to \\S 9.1.}"
)

with open("whitepaper.tex", "w") as f:
    f.write(text2)

print("done")
