package circuitbreaker

import (
	"context"
	"errors"
)

type Result uint8

const (
	Success Result = iota
	Failure
	Ignored
)

type Classifier func(err error) Result

func DefaultClassifier(err error) Result {
	switch {
	case err == nil:
		return Success
	case errors.Is(err, context.Canceled):
		return Ignored
	}
	return Failure
}
