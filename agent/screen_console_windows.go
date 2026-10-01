//go:build windows

package main

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"sync"
	"time"
)

// Windows may replace the physical console session at logoff. Keep the Screen
// stream/controller/frame counter in the service, and replace only its helper.
// Input is suppressed while the new console helper attaches; mode stays in mux.
type consoleDesktop struct {
	mu      sync.Mutex
	current *windowsDesktop
	events  <-chan message
	closed  bool

	launchHelper func(context.Context) (*windowsDesktop, error)
}

func launchDesktop(ctx context.Context) (desktopBridge, error) {
	launch, events := screenHelperFactory(launchConsoleHelper)
	d, err := launch(ctx)
	if err != nil {
		return nil, err
	}
	c := &consoleDesktop{current: d, events: events, launchHelper: launch}
	return c, nil
}
func (c *consoleDesktop) Events() <-chan message { return c.events }
func (c *consoleDesktop) Next(ctx context.Context) (desktopFrame, error) {
	for {
		c.mu.Lock()
		d := c.current
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return desktopFrame{}, context.Canceled
		}
		frame, err := d.Next(ctx)
		if err == nil {
			return frame, nil
		}
		if ctx.Err() != nil {
			return desktopFrame{}, ctx.Err()
		}
		if windows.WTSGetActiveConsoleSessionId() == d.session && (safeCaptureCode(err.Error()) || err.Error() == "capture_failed" || err.Error() == "input_failed" || err.Error() == "desktop_open_failed" || err.Error() == "desktop_switch_failed") {
			return desktopFrame{}, err
		}
		// Helper can be killed by logoff/session destruction. Kill old job and wait
		// a bounded interval for Windows to publish the next console, then relaunch.
		c.mu.Lock()
		c.current = nil
		c.mu.Unlock()
		d.Close()
		deadline := time.Now().Add(15 * time.Second)
		for {
			if ctx.Err() != nil {
				return desktopFrame{}, ctx.Err()
			}
			replacement, e := c.launchHelper(ctx)
			if e == nil {
				c.mu.Lock()
				c.current = replacement
				c.mu.Unlock()
				break
			}
			if time.Now().After(deadline) {
				return desktopFrame{}, errors.New("desktop_open_failed")
			}
			select {
			case <-ctx.Done():
				return desktopFrame{}, ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
}
func (c *consoleDesktop) Input(in screenInput) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil || c.closed || windows.WTSGetActiveConsoleSessionId() != c.current.session {
		return nil
	}
	return c.current.Input(in)
}
func (c *consoleDesktop) Operation(ctx context.Context, kind, text string) (string, error) {
	c.mu.Lock()
	d := c.current
	c.mu.Unlock()
	if d == nil {
		return "", errors.New("clipboard_unavailable")
	}
	return d.Operation(ctx, kind, text)
}
func (c *consoleDesktop) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.current != nil {
		c.current.Close()
	}
}
