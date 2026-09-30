package main

import (
	"errors"
	"unicode/utf16"
)

// The capture thread owns only desktops, never windows or hooks. Borrowed
// startup handles are never closed. Attach the replacement before closing old.
type desktopFollower struct {
	handle uintptr
	name   string
}

func (d *desktopFollower) follow(handle uintptr, name string, release func(), attach func(uintptr) bool, closeDesktop func(uintptr)) (bool, error) {
	if handle == 0 {
		return false, errors.New("desktop_open_failed")
	}
	// Unknown names may be followed, but must never reach diagnostics.
	if d.handle != 0 && name == d.name {
		closeDesktop(handle)
		return false, nil
	}
	release()
	if !attach(handle) {
		closeDesktop(handle)
		return false, errors.New("desktop_switch_failed")
	}
	old := d.handle
	d.handle, d.name = handle, name
	if old != 0 {
		closeDesktop(old)
	}
	return true, nil
}
func sasPolicyAllowsServices(value uint64, present bool) bool {
	return present && (value == 1 || value == 3)
}

// Snapshot releases before clearing ownership so a transition can retry them on
// the newly attached input desktop even if the old desktop rejected SendInput.
func releaseHeldScreenInput(keys, buttons map[int]bool) []screenInput {
	releases := make([]screenInput, 0, len(keys)+len(buttons))
	for key := range keys {
		releases = append(releases, screenInput{Action: "key", Key: key})
	}
	for button := range buttons {
		releases = append(releases, screenInput{Action: "button", Button: button})
	}
	clear(keys)
	clear(buttons)
	return releases
}

func clipboardUnicode(chars []uint16) (string, error) {
	end := 0
	for end < len(chars) && chars[end] != 0 {
		end++
	}
	if end == len(chars) {
		if end >= screenClipboardMax+1 {
			return "", errors.New("clipboard_too_large")
		}
		return "", errors.New("clipboard_unavailable")
	}
	for i := 0; i < end; i++ {
		if chars[i] >= 0xd800 && chars[i] <= 0xdbff {
			if i+1 == end || chars[i+1] < 0xdc00 || chars[i+1] > 0xdfff {
				return "", errors.New("clipboard_unavailable")
			}
			i++
		} else if chars[i] >= 0xdc00 && chars[i] <= 0xdfff {
			return "", errors.New("clipboard_unavailable")
		}
	}
	text := string(utf16.Decode(chars[:end]))
	if !clipboardText([]byte(text)) {
		return "", errors.New("clipboard_too_large")
	}
	return text, nil
}
