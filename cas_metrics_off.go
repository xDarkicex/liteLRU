//go:build !casmetrics

package liteLRU

// CASStats captures optional CAS contention and sweep counters when built with -tags casmetrics.
type CASStats struct {
	ClaimAttempts   uint64
	ClaimFailures   uint64
	SigCASAttempts  uint64
	SigCASFailures  uint64
	AgingSweeps     uint64
	VictimExhausted uint64
}

func casMetricAttempt()    {}
func casMetricFailure()    {}
func casMetricSigAttempt() {}
func casMetricSigFailure() {}
func casMetricAging()      {}
func casMetricExhausted()  {}

// ResetCASMetrics resets instrumentation counters (no-op unless built with -tags casmetrics).
func ResetCASMetrics() {}

// ReadCASMetrics returns current instrumentation counters (zero unless built with -tags casmetrics).
func ReadCASMetrics() CASStats { return CASStats{} }
