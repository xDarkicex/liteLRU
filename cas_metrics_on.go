//go:build casmetrics

package liteLRU

import "sync/atomic"

// CASStats captures exact CAS contention and sweep counters when built with -tags casmetrics.
type CASStats struct {
	ClaimAttempts   uint64
	ClaimFailures   uint64
	SigCASAttempts  uint64
	SigCASFailures  uint64
	AgingSweeps     uint64
	VictimExhausted uint64
}

var (
	casClaimAttempts   atomic.Uint64
	casClaimFailures   atomic.Uint64
	casSigAttempts     atomic.Uint64
	casSigFailures     atomic.Uint64
	casAgingSweeps     atomic.Uint64
	casVictimExhausted atomic.Uint64
)

func casMetricAttempt()    { casClaimAttempts.Add(1) }
func casMetricFailure()    { casClaimFailures.Add(1) }
func casMetricSigAttempt() { casSigAttempts.Add(1) }
func casMetricSigFailure() { casSigFailures.Add(1) }
func casMetricAging()      { casAgingSweeps.Add(1) }
func casMetricExhausted()  { casVictimExhausted.Add(1) }

// ResetCASMetrics zeroes all CAS contention counters.
func ResetCASMetrics() {
	casClaimAttempts.Store(0)
	casClaimFailures.Store(0)
	casSigAttempts.Store(0)
	casSigFailures.Store(0)
	casAgingSweeps.Store(0)
	casVictimExhausted.Store(0)
}

// ReadCASMetrics returns a snapshot of CAS contention counters.
func ReadCASMetrics() CASStats {
	return CASStats{
		ClaimAttempts:   casClaimAttempts.Load(),
		ClaimFailures:   casClaimFailures.Load(),
		SigCASAttempts:  casSigAttempts.Load(),
		SigCASFailures:  casSigFailures.Load(),
		AgingSweeps:     casAgingSweeps.Load(),
		VictimExhausted: casVictimExhausted.Load(),
	}
}
