package ratelimit

import (
	"fmt"

	"github.com/faultvane/faultvane-go"
)

var ErrRateLimited = fmt.Errorf("ratelimit: rate limit is reached: %w", faultvane.ErrRejected)
