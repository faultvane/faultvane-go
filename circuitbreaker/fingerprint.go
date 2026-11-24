package circuitbreaker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
)

const fingerprintVersion = "v1"

func (c config) canonical() string {
	return fmt.Sprintf("%s|failure_threshold=%d|minimum_calls=%d|failure_rate=%s|slow_call_rate=%s|slow_call_duration=%d|window_size=%d|window_buckets=%d|open_duration=%d|half_open_max_probes=%d|half_open_success_threshold=%d|probe_timeout=%d|state_ttl=%d",
		fingerprintVersion,
		c.thresholds.FailureThreshold,
		c.thresholds.MinimumCalls,
		strconv.FormatFloat(c.thresholds.FailureRate, 'g', -1, 64),
		strconv.FormatFloat(c.thresholds.SlowCallRate, 'g', -1, 64),
		c.slowCallDuration,
		c.windowSize,
		c.windowBuckets,
		c.openDuration,
		c.halfOpenMaxProbes,
		c.halfOpenSuccessThreshold,
		c.probeTimeout,
		c.stateTTL,
	)
}

func (c config) fingerprint() string {
	sum := sha256.Sum256([]byte(c.canonical()))
	return fingerprintVersion + ":" + hex.EncodeToString(sum[:8])
}
