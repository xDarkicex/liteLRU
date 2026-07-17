//go:build !amd64 && !arm64

package liteLRU

// CacheLineSize is set to 64 bytes as the industry standard for modern architectures.
const CacheLineSize = 64
