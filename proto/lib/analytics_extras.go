// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

// Fork-only (docs/client-analytics.md): limits shared by the server, which
// enforces them, and the SECO bridge, which splits and filters batches so a
// valid client never hits them.

const (
	// MaxClientLatencySamples bounds one ClientAnalyticsBatch.
	MaxClientLatencySamples = 512

	// MaxClientLatencyMs is the largest latency recorded (10 minutes): longer
	// means a suspended app or a wrong clock, not a slow server.
	MaxClientLatencyMs uint64 = 10 * 60 * 1000
)

// IsKnown reports whether m is a metric this build defines (not the zero
// value, and not one added by a newer peer).
func (m ClientLatencyMetric) IsKnown() bool {
	_, ok := ClientLatencyMetricRevMap[m]
	return ok
}
