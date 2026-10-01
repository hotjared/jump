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

// Desktop access rights from winuser.h. Playback access is required for
// synthetic input on Winlogon; do not request broader generic access or ACL rights.
const (
	desktopReadObjects     = 0x0001
	desktopJournalPlayback = 0x0020
	desktopWriteObjects    = 0x0080
)

func screenInputDesktopAccess() uint32 {
	return desktopReadObjects | desktopWriteObjects | desktopJournalPlayback
}

func desktopAttachmentReliable(querySucceeded, receivingInput bool) bool {
	return querySucceeded && receivingInput
}

func (d *desktopFollower) follow(handle uintptr, name string, oldReceivingInput bool, release func(), attach func(uintptr) bool, closeDesktop func(uintptr)) (bool, error) {
	if handle == 0 {
		return false, errors.New("desktop_open_failed")
	}
	// Unknown names may be followed, but must never reach diagnostics.
	// A name alone is not object identity. An inactive or unqueryable old handle
	// must be replaced even if Windows reused the name Default.
	if d.handle != 0 && oldReceivingInput && name == d.name {
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

func screenInputSafeAfterDesktopChange(in screenInput) bool {
	return in.Action == "move" || (in.Action == "key" || in.Action == "button") && !in.Down
}

// Retry at most once, after checking the actual input desktop. Never replay a
// press or wheel from the old desktop onto its replacement. A transition that
// prevents delivery is distinct from successful delivery, so held state is only
// recorded for events SendInput actually accepted.
func deliverDesktopInput(in screenInput, inject func(screenInput) bool, follow func() (bool, bool, error), release func()) (delivered, changed bool, err error) {
	if inject(in) {
		return true, false, nil
	}
	release()
	ready, changed, err := follow()
	if err != nil || !ready || changed && !screenInputSafeAfterDesktopChange(in) {
		return false, changed, err
	}
	if inject(in) {
		return true, changed, nil
	}
	release()
	ready, changedAgain, err := follow()
	changed = changed || changedAgain
	if err != nil || !ready || changed {
		return false, changed, err
	}
	return false, changed, errors.New("input_failed")
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

// Retain failed releases for the next attached desktop; do not claim delivery
// or drop ownership simply because SendInput was attempted.
func retryScreenReleases(keys, buttons map[int]bool, inject func(screenInput) bool) {
	for key := range keys {
		if inject(screenInput{Action: "key", Key: key}) {
			delete(keys, key)
		}
	}
	for button := range buttons {
		if inject(screenInput{Action: "button", Button: button}) {
			delete(buttons, button)
		}
	}
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
