import sys

with open("WHITEPAPER.md", "r") as f:
    text = f.read()

text = text.replace(
    "`memory.HashMap`",
    "`memory.HashMap` [15]" # I'll add the reference 15
)
# I need to add [15] to references, wait, reference 15 is MemC3 right now.
# It's better to just write the github link inline or add a new reference.
text = text.replace(
    "`memory.HashMap` [15]",
    "`memory.HashMap` (https://github.com/xDarkicex/memory)"
)

with open("WHITEPAPER.md", "w") as f:
    f.write(text)

with open("whitepaper.tex", "r") as f:
    text2 = f.read()

text2 = text2.replace(
    "\\texttt{memory.HashMap}",
    "\\texttt{memory.HashMap}\\footnote{\\url{https://github.com/xDarkicex/memory}}"
)

with open("whitepaper.tex", "w") as f:
    f.write(text2)

print("done")
