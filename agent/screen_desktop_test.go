package main

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestPreLoginConsoleDiscoveryAndSASPolicy(t *testing.T) {
	queried := false
	id, ok := discoverDesktopSession(func() uint32 { return 1 }, func(id uint32) bool { queried = true; return id == 1 })
	if !ok || id != 1 || !queried {
		t.Fatal("pre-login console rejected")
	}
	for _, invalid := range []uint32{0, 0xffffffff} {
		if _, ok := discoverDesktopSession(func() uint32 { return invalid }, func(uint32) bool { t.Fatal("queried invalid session"); return true }); ok {
			t.Fatal("invalid session")
		}
	}
	for i := uint64(0); i < 5; i++ {
		if sasPolicyAllowsServices(i, true) != (i == 1 || i == 3) {
			t.Fatal("SAS policy", i)
		}
	}
	if sasPolicyAllowsServices(1, false) {
		t.Fatal("missing policy allowed")
	}
}
func TestDesktopLifecycleTransitionsKeepAttachmentAndReleaseHeldInput(t *testing.T) {
	// UAC, SAS and lock use Winlogon; logged out starts there too.
	sequences := [][]string{
		{"Default", "Winlogon", "Default"}, {"Winlogon", "Default"},
		{"Default", "Winlogon", "Default"}, {"Default", "ScreenSaver", "Winlogon", "Default"},
		{"Winlogon", "Default"}, {"Default", "Winlogon"},
	}
	for _, sequence := range sequences {
		var follower desktopFollower
		var actions []string
		held := true
		for i, name := range sequence {
			actions = nil
			changed, err := follower.follow(uintptr(i+1), name, func() { held = false; actions = append(actions, "release") }, func(uintptr) bool { actions = append(actions, "attach"); return true }, func(uintptr) { actions = append(actions, "close") })
			expected := []string{"release", "attach"}
			if i > 0 {
				expected = append(expected, "close")
			}
			if !changed || err != nil || held || !reflect.DeepEqual(actions, expected) {
				t.Fatal(name, actions, err)
			}
			held = true
			actions = nil
			changed, err = follower.follow(100, name, func() { held = false }, func(uintptr) bool { t.Fatal("reattached unchanged desktop"); return true }, func(h uintptr) {
				if h != 100 {
					t.Fatal("closed current desktop")
				}
			})
			if changed || err != nil || !held {
				t.Fatal("unchanged desktop disturbed input")
			}
		}
	}
}
func TestDesktopFailedAttachPreservesOldOwnership(t *testing.T) {
	d := desktopFollower{handle: 1, name: "Default"}
	closed := uintptr(0)
	released := false
	_, err := d.follow(2, "Winlogon", func() { released = true }, func(uintptr) bool { return false }, func(h uintptr) { closed = h })
	if err == nil || err.Error() != "desktop_switch_failed" || d.handle != 1 || closed != 2 || !released {
		t.Fatal(d, closed, err)
	}
	_, err = d.follow(0, "", func() {}, func(uintptr) bool { return true }, func(uintptr) { t.Fatal("closed missing desktop") })
	if err == nil || screenFailure(err) != "desktop_open_failed" {
		t.Fatal("unsafe error mapping")
	}
}

func TestHeldKeysAndButtonsReleaseOnTransition(t *testing.T) {
	keys := map[int]bool{65: true, 162: true}
	buttons := map[int]bool{0: true, 2: true}
	releases := releaseHeldScreenInput(keys, buttons)
	if len(releases) != 4 || len(keys) != 0 || len(buttons) != 0 {
		t.Fatal("held input still owned")
	}
	seen := map[string]int{}
	for _, in := range releases {
		if in.Down || !validScreenInput(&in) {
			t.Fatal("not a valid release")
		}
		seen[in.Action]++
	}
	if seen["key"] != 2 || seen["button"] != 2 {
		t.Fatal("lost held input")
	}
}

func TestUnicodeClipboardWindowsHandoff(t *testing.T) {
	for _, text := range []string{"", "世界 😀", "x" + string(rune(0x10ffff)), strings.Repeat("x", screenClipboardMax)} {
		chars := append(utf16.Encode([]rune(text)), 0, 0, 0) // Include allocation padding.
		decoded, err := clipboardUnicode(chars)
		if err != nil || decoded != text {
			t.Fatal("Unicode handoff", err)
		}
	}
	for _, chars := range [][]uint16{{'x'}, {0xd800, 0}, {0xdc00, 0}, append(utf16.Encode([]rune(strings.Repeat("x", screenClipboardMax+1))), 0)} {
		if _, err := clipboardUnicode(chars); err == nil {
			t.Fatal("invalid/oversized Unicode accepted")
		}
	}
}
