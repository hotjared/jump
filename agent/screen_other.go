//go:build !windows

package main

import (
	"context"
	"errors"
)

func launchDesktop(context.Context) (desktopBridge, error) {
	return nil, errors.New("no_interactive_session")
}
func desktopHelper([]string) error { return errors.New("unsupported_agent") }
func screenSupported() bool        { return false }
