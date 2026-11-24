package circuitbreaker

import (
	"fmt"

	"github.com/faultvane/faultvane-go"
)

var (
	ErrCircuitOpen       = fmt.Errorf("circuitbreaker: circuit is open: %w", faultvane.ErrRejected)
	ErrProbeNotPermitted = fmt.Errorf("circuitbreaker: probe is not permitted: %w", faultvane.ErrRejected)
)
