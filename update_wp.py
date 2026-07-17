import re

with open("WHITEPAPER.md", "r") as f:
    content = f.read()

# Update the benchmark table in section 9.8
# The previous table had:
# | Configuration | Throughput (Req/sec) | p50 Latency | p99 Latency | Max Latency |
# |---------------|----------------------|-------------|-------------|-------------|
# | **No Cache**  | 86,468               | 303 µs      | 1.39 ms     | 19.3 ms     |
# | **`otter` v2**| **87,613**           | **274 µs**  | 1.37 ms     | 85.0 ms     |
# | **`liteLRU`** | 86,656               | 299 µs      | **1.33 ms** | **31.9 ms** |

old_table = """| Configuration | Throughput (Req/sec) | p50 Latency | p99 Latency | Max Latency |
|---------------|----------------------|-------------|-------------|-------------|
| **No Cache**  | 86,468               | 303 µs      | 1.39 ms     | 19.3 ms     |
| **`otter` v2**| **87,613**           | **274 µs**  | 1.37 ms     | 85.0 ms     |
| **`liteLRU`** | 86,656               | 299 µs      | **1.33 ms** | **31.9 ms** |"""

new_table = """| Configuration | Throughput (Req/sec) | p50 Latency | p99 Latency | Max Latency |
|---------------|----------------------|-------------|-------------|-------------|
| **No Cache**  | 81,619               | 322 µs      | 1.60 ms     | 119.4 ms    |
| **`otter` v2**| **90,396**           | 286 µs      | 1.33 ms     | **38.0 ms** |
| **`liteLRU`** | 89,738               | **271 µs**  | **1.25 ms** | 146.4 ms    |"""

content = content.replace(old_table, new_table)

with open("WHITEPAPER.md", "w") as f:
    f.write(content)

with open("whitepaper.tex", "r") as f:
    content = f.read()

old_tex = """No Cache & 86,468 & 303 $\\mu$s & 1.39 ms & 19.3 ms \\\\
\\texttt{otter} v2 & \\textbf{87,613} & \\textbf{274 $\\mu$s} & 1.37 ms & 85.0 ms \\\\
\\texttt{liteLRU} & 86,656 & 299 $\\mu$s & \\textbf{1.33 ms} & \\textbf{31.9 ms} \\\\"""

new_tex = """No Cache & 81,619 & 322 $\\mu$s & 1.60 ms & 119.4 ms \\\\
\\texttt{otter} v2 & \\textbf{90,396} & 286 $\\mu$s & 1.33 ms & \\textbf{38.0 ms} \\\\
\\texttt{liteLRU} & 89,738 & \\textbf{271 $\\mu$s} & \\textbf{1.25 ms} & 146.4 ms \\\\"""

content = content.replace(old_tex, new_tex)

with open("whitepaper.tex", "w") as f:
    f.write(content)

print("done")
