package faultvane

import (
	"context"
	"slices"
)

type Policy interface {
	Do(ctx context.Context, call func(context.Context) error) error
}

type pipeline []Policy

func Combine(policies ...Policy) Policy {
	return pipeline(slices.Clone(policies))
}

func (p pipeline) Do(ctx context.Context, call func(context.Context) error) error {
	if len(p) == 0 {
		return call(ctx)
	}
	return p[0].Do(ctx, func(ctx context.Context) error {
		return p[1:].Do(ctx, call)
	})
}

func Execute[T any](ctx context.Context, policy Policy, call func(context.Context) (T, error)) (T, error) {
	var value T
	err := policy.Do(ctx, func(ctx context.Context) error {
		var err error
		value, err = call(ctx)
		return err
	})
	return value, err
}
