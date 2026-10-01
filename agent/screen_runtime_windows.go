//go:build windows

package main

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows/svc/eventlog"
	"syscall"
)

// The interactive helper has no inherited console/stdout. Write these safe
// runtime records to the local Windows Application log under JumpAgent; no
// paths, input values, pixels, clipboard text, object handles or raw errors.
func newScreenRuntimeTrace() (*screenRuntimeTrace, func()) {
	log, err := eventlog.Open("JumpAgent")
	trace := &screenRuntimeTrace{}
	if err != nil {
		return trace, func() {}
	}
	trace.emit = func(d screenRuntimeDiagnostic) {
		text := fmt.Sprintf("Screen runtime stage=%s success=%t desktop=%s win32=%d code=%s", d.Stage, d.Success, d.Desktop, d.Win32, d.Code)
		if d.Success {
			_ = log.Info(1001, text)
		} else {
			_ = log.Warning(1001, text)
		}
	}
	return trace, func() { _ = log.Close() }
}

func screenWin32Code(err error) uint32 {
	var code syscall.Errno
	if errors.As(err, &code) {
		return uint32(code)
	}
	return 0
}
