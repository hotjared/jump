//go:build linux

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLinuxServiceInstallBehavior(t *testing.T) {
	state := filepath.Join(t.TempDir(), "identity.json")
	if err := os.WriteFile(state, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JUMP_AGENT_STATE", state)

	oldUID, oldCopy, oldWrite, oldSystemctl := linuxEffectiveUID, linuxCopyExecutable, linuxWriteFile, linuxSystemctl
	oldBinary, oldUnit := linuxBinaryTarget, linuxUnitTarget
	t.Cleanup(func() {
		linuxEffectiveUID, linuxCopyExecutable, linuxWriteFile, linuxSystemctl = oldUID, oldCopy, oldWrite, oldSystemctl
		linuxBinaryTarget, linuxUnitTarget = oldBinary, oldUnit
	})

	linuxEffectiveUID = func() int { return 0 }
	linuxBinaryTarget = "/test/jump-agent"
	linuxUnitTarget = "/test/jump-agent.service"
	var copied string
	linuxCopyExecutable = func(target string) error {
		copied = target
		return nil
	}
	var unit []byte
	linuxWriteFile = func(name string, data []byte, perm os.FileMode) error {
		if name != linuxUnitTarget || perm != 0644 {
			t.Fatalf("unexpected unit write: %s %o", name, perm)
		}
		unit = append([]byte(nil), data...)
		return nil
	}
	var commands [][]string
	linuxSystemctl = func(args ...string) error {
		commands = append(commands, append([]string(nil), args...))
		return nil
	}

	if err := serviceCommand([]string{"install"}); err != nil {
		t.Fatal(err)
	}
	if copied != linuxBinaryTarget {
		t.Fatalf("copied to %q, want %q", copied, linuxBinaryTarget)
	}
	if string(unit) != linuxSystemdUnit() {
		t.Fatal("installed unit content differs from generated unit")
	}
	want := [][]string{{"daemon-reload"}, {"enable", "--now", linuxServiceName}}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("systemctl commands = %#v, want %#v", commands, want)
	}
}

func TestLinuxServiceUninstallPreservesIdentity(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "identity.json")
	unit := filepath.Join(dir, "jump-agent.service")
	if err := os.WriteFile(state, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("unit"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JUMP_AGENT_STATE", state)

	oldUID, oldRemove, oldSystemctl, oldUnit := linuxEffectiveUID, linuxRemoveFile, linuxSystemctl, linuxUnitTarget
	t.Cleanup(func() {
		linuxEffectiveUID, linuxRemoveFile, linuxSystemctl, linuxUnitTarget = oldUID, oldRemove, oldSystemctl, oldUnit
	})
	linuxEffectiveUID = func() int { return 0 }
	linuxUnitTarget = unit
	linuxRemoveFile = os.Remove
	linuxSystemctl = func(args ...string) error { return nil }

	if err := serviceCommand([]string{"uninstall"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatalf("unit still present or unexpected error: %v", err)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("identity was removed by uninstall: %v", err)
	}
}
