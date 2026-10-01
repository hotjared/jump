package main

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const screenClipboardMax = 1024 * 1024
const screenClipboardWireMax = 24576

func safeDesktopName(name string) string {
	for _, value := range []string{"Default", "Winlogon", "ScreenSaver"} {
		if strings.EqualFold(name, value) {
			return value
		}
	}
	return ""
}
func safeScreenEvent(m message) bool {
	switch m.Stage {
	case "desktop_attached", "desktop_changed":
		return m.Desktop == "" || safeDesktopName(m.Desktop) == m.Desktop
	case "sas_requested", "sas_sent":
		return m.Desktop == ""
	}
	return false
}
func safeOperationCode(code string) bool {
	switch code {
	case "unsupported_agent", "ok", "control_required", "operation_busy", "operation_timeout", "operation_cancelled", "sas_blocked", "sas_unavailable", "clipboard_unavailable", "clipboard_too_large", "invalid_clipboard":
		return true
	}
	return false
}
func clipboardText(data []byte) bool {
	return len(data) <= screenClipboardMax && utf8.Valid(data) && !strings.ContainsRune(string(data), 0)
}

// One explicit operation, one credited chunk in flight. Buffers are bounded and
// contain transient clipboard text only; never log or persist this state.
type screenOperation struct {
	lastKind     string
	lastIndex    int
	lastID       string
	cancelled    bool
	id, kind     string
	started      time.Time
	index, count int
	waiting      bool
	data         []byte
}

func (o *screenOperation) clear() {
	last, kind, index := o.id, o.kind, o.index
	if last == "" {
		last, kind, index = o.lastID, o.lastKind, o.lastIndex
	}
	*o = screenOperation{lastID: last, lastKind: kind, lastIndex: index}
}
func validOperationEnvelope(m message) bool {
	if m.Input != nil || m.Mode != "" || m.FrameID != 0 || m.Width != 0 || m.Height != 0 || m.Stage != "" || m.Desktop != "" {
		return false
	}
	switch m.Type {
	case "screen_operation":
		return m.Data == "" && m.Code == "" && m.Index == 0 && m.Count == 0
	case "screen_clipboard":
		return m.Kind == "" && m.Code == ""
	case "screen_clipboard_ack":
		return m.Kind == "" && m.Code == "" && m.Data == "" && m.Count == 0 && m.Index >= 0
	case "screen_operation_result":
		return m.Data == "" && m.Index == 0 && m.Count == 0
	case "screen_operation_cancel":
		return m.Kind == "" && m.Code == "" && m.Data == "" && m.Index == 0 && m.Count == 0
	}
	return false
}
func (o *screenOperation) request(m message) error {
	if !validOperationEnvelope(m) || !validScreenID(m.RequestID) {
		return errors.New("invalid_clipboard")
	}
	bad := func() error { return errors.New("invalid_clipboard") }
	if m.Type == "screen_operation" {
		if o.id != "" || !validScreenID(m.RequestID) || (m.Kind != "sas" && m.Kind != "clipboard_set" && m.Kind != "clipboard_get") || m.Data != "" || m.Input != nil {
			return bad()
		}
		*o = screenOperation{id: m.RequestID, kind: m.Kind, started: time.Now()}
		return nil
	}
	if o.id == "" && m.RequestID == o.lastID {
		if m.Type == "screen_operation_cancel" || m.Type == "screen_clipboard_ack" && o.lastKind == "clipboard_get" && m.Index == o.lastIndex-1 {
			return nil
		}
	}
	if o.id == "" || m.RequestID != o.id {
		return bad()
	}
	switch m.Type {
	case "screen_operation_cancel":
		o.cancelled = true
		return nil
	case "screen_clipboard":
		if o.kind != "clipboard_set" {
			return bad()
		}
		return o.chunk(m)
	case "screen_clipboard_ack":
		if o.kind != "clipboard_get" {
			return bad()
		}
		return o.ack(m)
	}
	return bad()
}
func (o *screenOperation) response(m message) error {
	if !validOperationEnvelope(m) || !validScreenID(m.RequestID) {
		return errors.New("invalid_clipboard")
	}
	if o.id == "" || m.RequestID != o.id {
		return errors.New("invalid_clipboard")
	}
	if o.cancelled && m.Type != "screen_operation_result" {
		return nil
	}
	switch m.Type {
	case "screen_clipboard":
		if o.kind != "clipboard_get" {
			return errors.New("invalid_clipboard")
		}
		return o.chunk(m)
	case "screen_clipboard_ack":
		if o.kind != "clipboard_set" {
			return errors.New("invalid_clipboard")
		}
		return o.ack(m)
	case "screen_operation_result":
		if m.Kind != o.kind || !safeOperationCode(m.Code) || m.Data != "" {
			return errors.New("invalid_clipboard")
		}
		if !o.cancelled && m.Code == "ok" && o.kind != "sas" && (o.count == 0 || o.index != o.count || o.waiting) {
			return errors.New("invalid_clipboard")
		}
		o.clear()
		return nil
	}
	return errors.New("invalid_clipboard")
}
func (o *screenOperation) chunk(m message) error {
	bad := func() error { return errors.New("invalid_clipboard") }
	if o.waiting || m.Index != o.index || m.Count < 1 || m.Count > screenClipboardMax/screenChunkBytes || m.Index >= m.Count || len(m.Data) > 21848 || o.count != 0 && o.count != m.Count {
		return bad()
	}
	data, err := base64.StdEncoding.Strict().DecodeString(m.Data)
	if err != nil || len(data) > screenChunkBytes || m.Index < m.Count-1 && len(data) != screenChunkBytes || len(data) == 0 && m.Count != 1 || len(o.data)+len(data) > screenClipboardMax {
		return bad()
	}
	o.data = append(o.data, data...)
	o.count = m.Count
	o.index++
	o.waiting = true
	if o.index == o.count && !clipboardText(o.data) {
		return bad()
	}
	return nil
}
func (o *screenOperation) ack(m message) error {
	if !o.waiting || m.Index != o.index-1 || m.Data != "" {
		return errors.New("invalid_clipboard")
	}
	o.waiting = false
	return nil
}
func clipboardChunks(id string, data []byte) []message {
	count := max(1, (len(data)+screenChunkBytes-1)/screenChunkBytes)
	chunks := make([]message, 0, count)
	for i := 0; i < count; i++ {
		chunks = append(chunks, message{Type: "screen_clipboard", RequestID: id, Index: i, Count: count, Data: base64.StdEncoding.EncodeToString(data[i*screenChunkBytes : min(len(data), (i+1)*screenChunkBytes)])})
	}
	return chunks
}

func screenV2Message(kind string) bool {
	switch kind {
	case "screen_operation", "screen_clipboard", "screen_clipboard_ack", "screen_operation_cancel", "screen_operation_result", "screen_event":
		return true
	}
	return false
}
