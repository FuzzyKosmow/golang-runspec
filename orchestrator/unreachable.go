package orchestrator

import (
	"context"
	"errors"
)

// ErrTargetUnreachable marks a transport-level failure: the target never
// answered, as opposed to answering with an error.
var ErrTargetUnreachable = errors.New("target unreachable")

// Unreachable marks err as ErrTargetUnreachable without changing its text, so
// log and ES queries on the transport's own wording ("request timeout") still match.
func Unreachable(err error) error {
	if err == nil {
		return nil
	}
	return unreachableError{err}
}

type unreachableError struct{ err error }

func (e unreachableError) Error() string   { return e.err.Error() }
func (e unreachableError) Unwrap() []error { return []error{e.err, ErrTargetUnreachable} }

type stopOnUnreachableKey struct{}

// WithStopOnUnreachable makes Run skip a runner group when a guard dependency
// served by that runner failed with ErrTargetUnreachable, instead of sending
// the batch to the same dead target. Without it Run dispatches as before.
func WithStopOnUnreachable(ctx context.Context) context.Context {
	return context.WithValue(ctx, stopOnUnreachableKey{}, true)
}

func stopOnUnreachable(ctx context.Context) bool {
	on, _ := ctx.Value(stopOnUnreachableKey{}).(bool)
	return on
}
