package bulkhead

import (
	"fmt"

	"github.com/faultvane/faultvane-go"
)

var ErrFull = fmt.Errorf("bulkhead: all slots are in use: %w", faultvane.ErrRejected)
