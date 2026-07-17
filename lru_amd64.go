//go:build amd64

package liteLRU

// CacheLineSize is set to 64 bytes for x86_64 architectures.
const CacheLineSize = 64
