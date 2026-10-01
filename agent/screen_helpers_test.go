package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConsoleHelperReplacementKeepsEventStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type helper struct {
		events chan<- message
		stop   context.CancelFunc
		done   <-chan struct{}
	}
	var attempts int
	var shared chan message
	launch, events := screenHelperFactory(func(ctx context.Context, events chan message) (*helper, error) {
		attempts++
		if shared == nil {
			shared = events
		} else if events != shared {
			t.Fatal("replacement received a different event channel")
		}
		if attempts == 2 {
			return nil, errors.New("helper_start_failed")
		}
		readerCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		started := make(chan struct{})
		go func() {
			defer close(done)
			// Model IPC producing an event before launch returns.
			select {
			case events <- message{Type: "screen_event", Stage: "desktop_attached", Desktop: "Winlogon"}:
			case <-readerCtx.Done():
			}
			close(started)
			<-readerCtx.Done()
		}()
		<-started
		return &helper{events: events, stop: stop, done: done}, nil
	})
	for generation := 0; generation < 3; generation++ {
		h, err := launch(ctx)
		if generation == 1 {
			if err == nil {
				t.Fatal("expected failed replacement attempt")
			}
			h, err = launch(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
		select {
		case event, open := <-events:
			if !open || event.Stage != "desktop_attached" || event.Desktop != "Winlogon" {
				t.Fatal("early helper event lost or shared stream closed")
			}
		case <-ctx.Done():
			t.Fatal("missing event from helper generation", generation)
		}
		h.stop()
		<-h.done
		// Stopping a helper leaves the stream usable for its replacement.
		h.events <- message{Type: "screen_event", Stage: "desktop_changed", Desktop: "Default"}
		if event, open := <-events; !open || event.Stage != "desktop_changed" {
			t.Fatal("helper cleanup closed the Screen event stream")
		}
	}
}
