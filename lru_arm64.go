//go:build arm64

package liteLRU

// CacheLineSize is set to 128 bytes for ARM64 architectures (e.g., Apple Silicon M-series L2/L3).
const CacheLineSize = 128
