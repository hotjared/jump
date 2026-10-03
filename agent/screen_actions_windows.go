//go:build windows

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"runtime"
	"unicode/utf16"
	"unsafe"
)

func (d *windowsDesktop) Events() <-chan message { return d.events }
func (d *windowsDesktop) Operation(ctx context.Context, kind, text string) (string, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if windows.WTSGetActiveConsoleSessionId() != d.session {
		return "", errors.New("clipboard_unavailable")
	}
	if kind == "sas" {
		select {
		case d.events <- message{Type: "screen_event", Stage: "sas_requested"}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		err := sendConsoleSAS(d.session)
		if err == nil {
			select {
			case d.events <- message{Type: "screen_event", Stage: "sas_sent"}:
			case <-ctx.Done():
			}
		}
		return "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errors.New("clipboard_unavailable")
	}
	id := hex.EncodeToString(random[:4]) + "-" + hex.EncodeToString(random[4:6]) + "-" + hex.EncodeToString(random[6:8]) + "-" + hex.EncodeToString(random[8:10]) + "-" + hex.EncodeToString(random[10:])
	request := message{Type: "screen_operation", RequestID: id, Kind: kind}
	var op screenOperation
	if op.request(request) != nil {
		return "", errors.New("invalid_clipboard")
	}
	d.resultMu.Lock()
	d.resultID, d.resultContext = id, ctx
	d.resultMu.Unlock()
	defer func() { d.resultMu.Lock(); d.resultID = ""; d.resultContext = nil; d.resultMu.Unlock() }()
	if err := d.pipe.send(request); err != nil {
		return "", err
	}
	if kind == "clipboard_set" {
		if !clipboardText([]byte(text)) {
			return "", errors.New("clipboard_too_large")
		}
		for _, chunk := range clipboardChunks(id, []byte(text)) {
			if op.request(chunk) != nil {
				return "", errors.New("invalid_clipboard")
			}
			if err := d.pipe.send(chunk); err != nil {
				return "", err
			}
			_ = op.response(message{Type: "screen_clipboard_ack", RequestID: id, Index: chunk.Index})
		}
	}
	for {
		select {
		case <-ctx.Done():
			return "", errors.New("operation_timeout")
		case <-d.pipe.ctx.Done():
			return "", errors.New("clipboard_unavailable")
		case reply := <-d.results:
			if reply.RequestID != id {
				continue
			}
			data := op.data
			if op.response(reply) != nil {
				return "", errors.New("invalid_clipboard")
			}
			if reply.Type == "screen_clipboard" {
				_ = op.request(message{Type: "screen_clipboard_ack", RequestID: id, Index: reply.Index})
				continue
			}
			if reply.Code != "ok" {
				return "", errors.New(reply.Code)
			}
			return string(data), nil
		}
	}
}

// SendSAS returns void. Preflight the service policy and bound physical console,
// then call directly from the LocalSystem service. Impersonating a copy of the
// service token with a rewritten session ID can silently prevent SAS delivery.
// Success means submitted, not observed sign-in completion.
func sendConsoleSAS(session uint32) error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Microsoft\Windows\CurrentVersion\Policies\System`, registry.QUERY_VALUE)
	if err != nil {
		return errors.New("sas_blocked")
	}
	value, valueType, err := key.GetIntegerValue("SoftwareSASGeneration")
	key.Close()
	if !sasPolicyAllowsServices(value, err == nil && valueType == registry.DWORD) {
		return errors.New("sas_blocked")
	}
	proc := windows.NewLazySystemDLL("sas.dll").NewProc("SendSAS")
	if proc.Find() != nil {
		return errors.New("sas_unavailable")
	}
	if windows.WTSGetActiveConsoleSessionId() != session {
		return errors.New("sas_unavailable")
	}
	proc.Call(0)
	return nil
}

// A real message-only window owns the clipboard on a separate locked thread.
// The capture/input thread remains free of windows/hooks for SetThreadDesktop.
// Data is eagerly rendered; no listener or automatic synchronization exists.
func windowsClipboard(kind, text string) (string, error) {
	fail := func() (string, error) { return "", errors.New("clipboard_unavailable") }
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, _, _ := screenUser32.NewProc("GetThreadDesktop").Call(uintptr(windows.GetCurrentThreadId()))
	desktop, _, _ := screenUser32.NewProc("OpenInputDesktop").Call(0, 0, 0x83) // READ/WRITEOBJECTS + CREATEWINDOW
	if desktop == 0 {
		return fail()
	}
	attached, _, _ := screenUser32.NewProc("SetThreadDesktop").Call(desktop)
	if attached == 0 {
		screenUser32.NewProc("CloseDesktop").Call(desktop)
		return fail()
	}
	defer func() {
		r, _, _ := screenUser32.NewProc("SetThreadDesktop").Call(original)
		if r != 0 {
			screenUser32.NewProc("CloseDesktop").Call(desktop)
		}
	}()
	window, _, _ := screenUser32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("STATIC"))), 0, 0, 0, 0, 0, 0, ^uintptr(2), 0, 0, 0)
	if window == 0 {
		return fail()
	}
	defer screenUser32.NewProc("DestroyWindow").Call(window)
	opened, _, _ := screenUser32.NewProc("OpenClipboard").Call(window)
	if opened == 0 {
		return fail()
	}
	defer screenUser32.NewProc("CloseClipboard").Call()
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	if kind == "clipboard_set" {
		if !clipboardText([]byte(text)) {
			return "", errors.New("clipboard_too_large")
		}
		chars := append(utf16.Encode([]rune(text)), 0)
		memory, _, _ := kernel.NewProc("GlobalAlloc").Call(0x42, uintptr(len(chars)*2)) // MOVEABLE | ZEROINIT
		if memory == 0 {
			return fail()
		}
		owned := true
		defer func() {
			if owned {
				kernel.NewProc("GlobalFree").Call(memory)
			}
		}()
		pointer, _, _ := kernel.NewProc("GlobalLock").Call(memory)
		if pointer == 0 {
			return fail()
		}
		copy(unsafe.Slice((*uint16)(unsafe.Pointer(pointer)), len(chars)), chars)
		kernel.NewProc("GlobalUnlock").Call(memory)
		emptied, _, _ := screenUser32.NewProc("EmptyClipboard").Call()
		if emptied == 0 {
			return fail()
		}
		result, _, _ := screenUser32.NewProc("SetClipboardData").Call(13, memory)
		if result == 0 {
			return fail()
		}
		owned = false // Windows owns memory only after successful SetClipboardData.
		return "", nil
	}
	available, _, _ := screenUser32.NewProc("IsClipboardFormatAvailable").Call(13)
	if available == 0 {
		return "", nil
	}
	memory, _, _ := screenUser32.NewProc("GetClipboardData").Call(13)
	if memory == 0 {
		return fail()
	}
	size, _, _ := kernel.NewProc("GlobalSize").Call(memory)
	if size < 2 {
		return fail()
	}

	pointer, _, _ := kernel.NewProc("GlobalLock").Call(memory)
	if pointer == 0 {
		return fail()
	}
	defer kernel.NewProc("GlobalUnlock").Call(memory)
	// GlobalSize may include allocator rounding or unused capacity. Inspect only
	// a bounded prefix; judge the actual text's UTF-8 bytes, not allocation size.
	chars := unsafe.Slice((*uint16)(unsafe.Pointer(pointer)), min(int(size/2), screenClipboardMax+1))
	return clipboardUnicode(chars)
}
