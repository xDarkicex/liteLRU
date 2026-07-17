import numpy as np

N = 100000
s = 0.8
num_ops = 2000000

print("Generating CDF...")
ranks = np.arange(1, N+1)
pmf = 1.0 / np.power(ranks, s)
pmf /= np.sum(pmf)
cdf = np.cumsum(pmf)

print("Sampling...")
r = np.random.rand(num_ops)
# searchsorted finds the index where r would be inserted
samples = np.searchsorted(cdf, r)

print("Writing to file...")
with open("zipf_0.8.bin", "wb") as f:
    samples.astype(np.int32).tofile(f)

print("Done")
