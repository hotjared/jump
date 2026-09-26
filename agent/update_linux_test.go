//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxAtomicReplaceAndRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		dir := t.TempDir()
		target, staged := filepath.Join(dir, "agent"), filepath.Join(dir, "staged")
		backup := filepath.Join(dir, "backup")
		os.WriteFile(target, []byte("old"), 0700)
		os.WriteFile(staged, []byte("new"), 0700)
		starts, stops := 0, 0
		err := replaceLinuxBinary(target, staged, backup,
			func() error { starts++; return nil },
			func() error { stops++; return nil },
			func() bool { return !fail })
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if fail && (string(got) != "old" || starts != 2 || stops != 1) {
			t.Fatalf("rollback: %s", got)
		}
		if !fail && (string(got) != "new" || starts != 1 || stops != 0) {
			t.Fatalf("replace: %s", got)
		}
	}
}
