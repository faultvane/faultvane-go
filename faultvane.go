package faultvane

import "errors"

var (
	ErrRejected         = errors.New("faultvane: call rejected")
	ErrInvalidConfig    = errors.New("faultvane: invalid configuration")
	ErrStoreUnavailable = errors.New("faultvane: store unavailable")
)
