import sys

with open("WHITEPAPER.md", "r") as f:
    text = f.read()

text = text.replace(
    "To stress the concurrent write protocol under severe contention",
    "We compare against Otter v2 rather than ristretto here because ristretto's hit-rate collapses under intense concurrent pressure (as seen in §9.6), rendering its write throughput an artifact of dropped samples rather than true admission. To stress the concurrent write protocol under severe contention"
)

with open("WHITEPAPER.md", "w") as f:
    f.write(text)

with open("whitepaper.tex", "r") as f:
    text2 = f.read()

text2 = text2.replace(
    "To stress the concurrent write protocol under severe contention",
    "We compare against Otter v2 rather than \\texttt{ristretto} here because \\texttt{ristretto}'s hit-rate collapses under intense concurrent pressure (as seen in \\S 9.6), rendering its write throughput an artifact of dropped samples rather than true admission. To stress the concurrent write protocol under severe contention"
)

with open("whitepaper.tex", "w") as f:
    f.write(text2)

print("done")
