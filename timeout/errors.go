package timeout

import "errors"

var ErrTimeout = errors.New("timeout: call exceeded the time limit")
