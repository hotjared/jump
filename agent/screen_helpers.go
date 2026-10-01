package main

import "context"

// The Screen stream owns one event channel across all console helpers. Supply
// it during construction, before a helper can start its IPC reader. Individual
// helpers must not close this channel when they stop or are replaced.
func screenHelperFactory[T any](launch func(context.Context, chan message) (T, error)) (func(context.Context) (T, error), <-chan message) {
	events := make(chan message, 16)
	return func(ctx context.Context) (T, error) {
		return launch(ctx, events)
	}, events
}
