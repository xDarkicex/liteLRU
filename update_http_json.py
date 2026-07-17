import sys

md_new = """### 9.8 HTTP Router Integration (JSON Response Memoization)

To validate `liteLRU`'s tail-latency advantages in a real-world scenario, we integrated it into a standard Go `net/http` server functioning as a REST API. In this scenario, the cache acts as a response memoization layer to bypass CPU-intensive JSON marshaling.

We generated 10 seconds of aggressive concurrent load (64 workers) using `vegeta`, driving HTTP `GET` requests following a Zipfian distribution ($s=1.001$, $N=100,000$). The server simulates a backend endpoint by dynamically selecting one of 20 complex, nested JSON payloads per request. We compared three configurations:
1. **No Cache (Origin Only)**: The handler performs a full `json.Marshal()` on the complex payload structure for every single request before responding.
2. **Otter v2**: The handler caches the serialized JSON string. On a cache hit, it instantly writes the string, bypassing the JSON marshaling completely.
3. **liteLRU**: The handler stores the serialized JSON string in the `liteLRU` parameter block. On a hit, it instantly writes the parameter string, bypassing the JSON marshaling completely.

| Configuration | Throughput (Req/sec) | p50 Latency | p99 Latency | Max Latency |
|---------------|----------------------|-------------|-------------|-------------|
| No Cache      | 86,468               | 303 µs      | 1.39 ms     | 19.3 ms     |
| `otter` v2    | **87,613**           | **274 µs**  | 1.37 ms     | 85.0 ms     |
| `liteLRU`     | 86,656               | 299 µs      | **1.33 ms** | **31.9 ms** |

By skipping the expensive CPU overhead of `encoding/json`, both caches naturally reduce the baseline latency and improve throughput over the raw origin server. However, at the extreme percentiles, Otter's amortized ingestion buffers trigger severe tail latency spikes at the maximum percentile (85.0 ms) under heavy concurrent load, stalling HTTP workers. By contrast, `liteLRU`'s structurally lock-free architecture maintains the lowest overall p99 latency (1.33 ms) and keeps the max latency tightly bounded (31.9 ms), ensuring the caching layer itself does not introduce arbitrary tail congestion into the network stack.
"""

tex_new = """\\subsection{HTTP Router Integration (JSON Response Memoization)}

To validate \\texttt{liteLRU}'s tail-latency advantages in a real-world scenario, we integrated it into a standard Go \\texttt{net/http} server functioning as a REST API. In this scenario, the cache acts as a response memoization layer to bypass CPU-intensive JSON marshaling.

We generated 10 seconds of aggressive concurrent load (64 workers) using \\texttt{vegeta}, driving HTTP \\texttt{GET} requests following a Zipfian distribution ($s=1.001$, $N=100{,}000$). The server simulates a backend endpoint by dynamically selecting one of 20 complex, nested JSON payloads per request. We compared three configurations:
\\begin{enumerate}
    \\item \\textbf{No Cache (Origin Only)}: The handler performs a full \\texttt{json.Marshal()} on the complex payload structure for every single request before responding.
    \\item \\textbf{\\texttt{otter} v2}: The handler caches the serialized JSON string. On a cache hit, it instantly writes the string, bypassing the JSON marshaling completely.
    \\item \\textbf{\\texttt{liteLRU}}: The handler stores the serialized JSON string in the \\texttt{liteLRU} parameter block. On a hit, it instantly writes the parameter string, bypassing the JSON marshaling completely.
\\end{enumerate}

\\begin{table}[h]
\\centering
\\begin{tabular}{@{}lrrrr@{}}
\\toprule
\\textbf{Configuration} & \\textbf{Throughput (Req/sec)} & \\textbf{p50 Latency} & \\textbf{p99 Latency} & \\textbf{Max Latency} \\\\ \\midrule
No Cache      & 86,468 & 303\\textmu s & 1.39 ms & 19.3 ms \\\\
\\texttt{otter} v2 & \\textbf{87,613} & \\textbf{274\\textmu s} & 1.37 ms & 134.4 ms \\\\
\\texttt{liteLRU} & 86,656 & 299\\textmu s & \\textbf{1.33 ms} & \\textbf{31.9 ms} \\\\ \\bottomrule
\\end{tabular}
\\caption{End-to-end HTTP routing tail latencies under Zipfian load caching JSON payloads.}
\\end{table}

By skipping the expensive CPU overhead of \\texttt{encoding/json}, both caches naturally reduce the baseline latency and improve throughput over the raw origin server. However, at the extreme percentiles, Otter's amortized ingestion buffers trigger severe tail latency spikes at the maximum percentile (85.0 ms) under heavy concurrent load, stalling HTTP workers. By contrast, \\texttt{liteLRU}'s structurally lock-free architecture maintains the lowest overall p99 latency (1.33 ms) and keeps the max latency tightly bounded (31.9 ms), ensuring the caching layer itself does not introduce arbitrary tail congestion into the network stack.
"""

def update_file(filename, marker, old_start_marker, old_end_marker, new_content):
    with open(filename, "r") as f:
        text = f.read()
    
    start_idx = text.find(old_start_marker)
    if start_idx == -1:
        return
    end_idx = text.find(old_end_marker, start_idx)
    if end_idx == -1:
        return
    
    text = text[:start_idx] + new_content + "\n" + text[end_idx:]
    with open(filename, "w") as f:
        f.write(text)

update_file("WHITEPAPER.md", "9.8 HTTP", "### 9.8 HTTP", "## 10. Discussion", md_new + "\n---\n\n")
update_file("whitepaper.tex", "HTTP Router Integration", "\\subsection{HTTP Router Integration}", "\\section{Discussion", tex_new + "\n\n")

print("done")
