package loadshed

import (
	"fmt"

	"github.com/faultvane/faultvane-go"
)

var ErrShed = fmt.Errorf("loadshed: service is overloaded: %w", faultvane.ErrRejected)
