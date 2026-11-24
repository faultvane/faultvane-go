package concurrencylimit

import (
	"fmt"

	"github.com/faultvane/faultvane-go"
)

var ErrLimitExceeded = fmt.Errorf("concurrencylimit: limit of concurrent calls is exceeded: %w", faultvane.ErrRejected)
