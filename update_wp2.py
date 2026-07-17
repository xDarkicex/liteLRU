import re

with open("WHITEPAPER.md", "r") as f:
    content = f.read()

old_table = """| Configuration | Throughput (Req/sec) | p50 Latency | p99 Latency | Max Latency |
|---------------|----------------------|-------------|-------------|-------------|
| **No Cache**  | 81,619               | 322 µs      | 1.60 ms     | 119.4 ms    |
| **`otter` v2**| **90,396**           | 286 µs      | 1.33 ms     | **38.0 ms** |
| **`liteLRU`** | 89,738               | **271 µs**  | **1.25 ms** | 146.4 ms    |"""

new_table = """| Configuration | Throughput (Req/sec) | p50 Latency | p99 Latency | Max Latency |
|---------------|----------------------|-------------|-------------|-------------|
| **No Cache**  | 86,453               | 303 µs      | 1.36 ms     | 35.1 ms     |
| **`otter` v2**| 86,056               | 279 µs      | 1.41 ms     | 173.1 ms    |
| **`liteLRU`** | **93,603**           | **276 µs**  | **1.20 ms** | **43.1 ms** |"""

if old_table in content:
    content = content.replace(old_table, new_table)
else:
    print("WARNING: Could not find old_table in markdown!")

# Also update the abstract or architecture sections if they mention HashMaps
content = content.replace("open-addressed lock-free hash map", "64-way set associative SWAR (SIMD Within A Register) architecture")
content = content.replace("hash map lookup", "SWAR signature lookup")
content = content.replace("global index map", "set associative routing")
content = content.replace("memory.HashMap", "64-way set associativity")

with open("WHITEPAPER.md", "w") as f:
    f.write(content)


with open("whitepaper.tex", "r") as f:
    content = f.read()

old_tex = """No Cache & 81,619 & 322 $\\mu$s & 1.60 ms & 119.4 ms \\\\
\\texttt{otter} v2 & \\textbf{90,396} & 286 $\\mu$s & 1.33 ms & \\textbf{38.0 ms} \\\\
\\texttt{liteLRU} & 89,738 & \\textbf{271 $\\mu$s} & \\textbf{1.25 ms} & 146.4 ms \\\\"""

new_tex = """No Cache & 86,453 & 303 $\\mu$s & 1.36 ms & 35.1 ms \\\\
\\texttt{otter} v2 & 86,056 & 279 $\\mu$s & 1.41 ms & 173.1 ms \\\\
\\texttt{liteLRU} & \\textbf{93,603} & \\textbf{276 $\\mu$s} & \\textbf{1.20 ms} & \\textbf{43.1 ms} \\\\"""

if old_tex in content:
    content = content.replace(old_tex, new_tex)
else:
    print("WARNING: Could not find old_tex in latex!")

content = content.replace("open-addressed lock-free hash map", "64-way set associative SWAR (SIMD Within A Register) architecture")
content = content.replace("hash map lookup", "SWAR signature lookup")
content = content.replace("global index map", "set associative routing")
content = content.replace("memory.HashMap", "64-way set associativity")

with open("whitepaper.tex", "w") as f:
    f.write(content)

print("done")
