package loadshed

import "context"

type Priority int

const (
	Critical Priority = iota
	High
	Normal
	Low
)

const priorities = 4

type priorityKey struct{}

func (p Priority) valid() bool {
	return p >= Critical && p <= Low
}

func WithPriority(ctx context.Context, p Priority) context.Context {
	return context.WithValue(ctx, priorityKey{}, p)
}

func PriorityFrom(ctx context.Context) Priority {
	if p, found := ctx.Value(priorityKey{}).(Priority); found && p.valid() {
		return p
	}
	return Normal
}
