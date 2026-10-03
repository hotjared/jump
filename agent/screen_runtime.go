package main

// Local runtime diagnostics only: never sent to the browser, Diagnostics or
// Audit. Keep one last value per fixed stage, rather than logging every frame
// or input event. Unknown desktop names and failure strings are discarded.
type screenRuntimeDiagnostic struct {
	Stage, Desktop, Code string
	Success              bool
	Win32                uint32
}

type screenRuntimeTrace struct {
	last map[string]screenRuntimeDiagnostic
	emit func(screenRuntimeDiagnostic)
}

func (t *screenRuntimeTrace) resetCapture() {
	for _, stage := range []string{"desktop_changed", "capture_first_attempt", "capture_first_result", "frame_first_sent"} {
		delete(t.last, stage)
	}
}

func (t *screenRuntimeTrace) record(stage, desktop string, success bool, win32 uint32, code string) {
	switch stage {
	case "desktop_open", "desktop_name", "desktop_old_input", "desktop_old_active", "desktop_attach", "desktop_thread", "desktop_changed", "capture_first_attempt", "capture_first_result", "frame_first_sent", "send_input":
	default:
		return
	}
	if !safeCaptureCode(code) {
		code = ""
	}
	if success {
		win32 = 0 // LastError on a successful Windows call may be stale.
	}
	d := screenRuntimeDiagnostic{Stage: stage, Desktop: safeDesktopName(desktop), Code: code, Success: success, Win32: win32}
	if t.last == nil {
		t.last = make(map[string]screenRuntimeDiagnostic)
	}
	if previous, ok := t.last[stage]; ok && previous == d {
		return
	}
	t.last[stage] = d
	if t.emit != nil {
		t.emit(d)
	}
}
