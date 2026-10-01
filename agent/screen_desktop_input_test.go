package main

import (
	"errors"
	"reflect"
	"testing"
)

func TestScreenInputDesktopAccessIsMinimalPlaybackMask(t *testing.T) {
	// Check SDK bit values independently of the implementation constants.
	if mask := screenInputDesktopAccess(); mask != 0x0001|0x0020|0x0080 {
		t.Fatalf("input desktop access = %#x; need READOBJECTS, JOURNALPLAYBACK and WRITEOBJECTS only", mask)
	}
}

func TestUnreliableSameNamedDesktopMustBeReattached(t *testing.T) {
	for _, status := range []struct {
		name           string
		queried, input bool
	}{
		{"failed_old_UOI_IO", false, true},
		{"inactive_old_handle", true, false},
	} {
		t.Run(status.name, func(t *testing.T) {
			d := desktopFollower{handle: 1, name: "Default"}
			var actions []string
			changed, err := d.follow(2, "Default", desktopAttachmentReliable(status.queried, status.input),
				func() { actions = append(actions, "release") },
				func(h uintptr) bool {
					if h != 2 {
						t.Fatal("wrong replacement attached")
					}
					actions = append(actions, "attach")
					return true
				},
				func(h uintptr) {
					if h != 1 {
						t.Fatal("new same-named desktop discarded")
					}
					actions = append(actions, "close_old")
				})
			if !changed || err != nil || d.handle != 2 || d.name != "Default" || !reflect.DeepEqual(actions, []string{"release", "attach", "close_old"}) {
				t.Fatal(d, actions, err)
			}
		})
	}
}

func TestDesktopInputRecoveryIsBoundedAndDoesNotReplayPresses(t *testing.T) {
	for _, tc := range []struct {
		name             string
		input            screenInput
		ready, changed   bool
		retrySucceeds    bool
		attempts         int
		delivered, fails bool
	}{
		{"transient_same_desktop", screenInput{Action: "move"}, true, false, true, 2, true, false},
		{"persistent_failure", screenInput{Action: "move"}, true, false, false, 2, false, true},
		{"unavailable_desktop", screenInput{Action: "move"}, false, false, false, 1, false, false},
		{"move_to_new_desktop", screenInput{Action: "move"}, true, true, true, 2, true, false},
		{"failed_move_during_transition", screenInput{Action: "move"}, true, true, false, 2, false, false},
		{"old_key_down", screenInput{Action: "key", Key: 65, Down: true}, true, true, true, 1, false, false},
		{"old_button_down", screenInput{Action: "button", Down: true}, true, true, true, 1, false, false},
		{"old_wheel", screenInput{Action: "wheel", Delta: 120}, true, true, true, 1, false, false},
		{"release_on_new_desktop", screenInput{Action: "key", Key: 65}, true, true, true, 2, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts, releases, follows := 0, 0, 0
			delivered, changed, err := deliverDesktopInput(tc.input,
				func(screenInput) bool { attempts++; return attempts == 2 && tc.retrySucceeds },
				func() (bool, bool, error) {
					follows++
					if releases == 0 {
						t.Fatal("held input not released before recovery")
					}
					return tc.ready, tc.changed, nil
				},
				func() { releases++ })
			if attempts != tc.attempts || follows > 2 || delivered != tc.delivered || changed != tc.changed || (err != nil) != tc.fails {
				t.Fatal(attempts, follows, delivered, changed, err)
			}
			if err != nil && err.Error() != "input_failed" {
				t.Fatal("unsafe input error")
			}
		})
	}
}

func TestSecondInputFailureDuringDesktopSwapDoesNotPoisonStream(t *testing.T) {
	follows, attempts := 0, 0
	delivered, changed, err := deliverDesktopInput(screenInput{Action: "move"},
		func(screenInput) bool { attempts++; return false },
		func() (bool, bool, error) { follows++; return true, follows == 2, nil }, func() {})
	if delivered || !changed || err != nil || attempts != 2 || follows != 2 {
		t.Fatal(delivered, changed, err, attempts, follows)
	}
}

func TestInputRecoveryPreservesSpecificDesktopFailures(t *testing.T) {
	for _, code := range []string{"desktop_open_failed", "desktop_switch_failed"} {
		_, _, err := deliverDesktopInput(screenInput{Action: "move"}, func(screenInput) bool { return false },
			func() (bool, bool, error) { return false, false, errors.New(code) }, func() {})
		if err == nil || screenFailure(err) != code {
			t.Fatal("desktop failure collapsed into input failure", err)
		}
	}
}

func TestFailedHeldInputReleasesRemainPendingUntilAccepted(t *testing.T) {
	keys, buttons := map[int]bool{65: true}, map[int]bool{0: true}
	actualCalls := 0
	inject := func(in screenInput) bool {
		actualCalls++
		if in.Down {
			t.Fatal("release retried a press")
		}
		return actualCalls > 2
	}
	retryScreenReleases(keys, buttons, inject)
	if len(keys) != 1 || len(buttons) != 1 || actualCalls != 2 {
		t.Fatal("failed releases lost or retried indefinitely")
	}
	retryScreenReleases(keys, buttons, inject)
	if len(keys) != 0 || len(buttons) != 0 || actualCalls != 4 {
		t.Fatal("accepted releases still held")
	}
}
