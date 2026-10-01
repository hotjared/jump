package main

import "testing"

func TestScreenRuntimeDiagnosticsAreSafeAndBounded(t *testing.T) {
	var records []screenRuntimeDiagnostic
	trace := &screenRuntimeTrace{emit: func(d screenRuntimeDiagnostic) { records = append(records, d) }}
	trace.record("arbitrary window title", "secret desktop name", false, 5, "clipboard contents")
	if len(records) != 0 || len(trace.last) != 0 {
		t.Fatal("unrestricted diagnostic stage accepted")
	}
	for i := 0; i < 100; i++ {
		trace.record("desktop_old_input", "secret desktop name", false, 6, "credentials")
	}
	if len(records) != 1 || records[0].Desktop != "" || records[0].Code != "" || records[0].Win32 != 6 {
		t.Fatal("unsafe values or per-frame log spam", records)
	}
	trace.record("desktop_old_input", "Default", true, 999, "")
	if len(records) != 2 || records[1].Win32 != 0 || records[1].Desktop != "Default" {
		t.Fatal("query recovery hidden or stale Windows error logged", records)
	}
	trace.record("capture_first_result", "Default", false, 0, "capture_blit_failed")
	if records[2].Code != "capture_blit_failed" {
		t.Fatal("specific safe capture stage lost")
	}
	trace.record("capture_first_attempt", "Default", true, 0, "")
	trace.resetCapture()
	trace.record("capture_first_attempt", "Default", true, 0, "")
	if len(records) != 5 {
		t.Fatal("first capture after same-name reattachment hidden")
	}
}
