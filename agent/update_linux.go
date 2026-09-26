//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func installedAgentPath() string { return linuxInstallPath }

func launchUpdateHelper(operation, checksum string) error {
	helper := linuxInstallPath + ".helper-" + operation
	defer func() { /* helper removes itself after running */ }()
	if err := copyCurrentTo(helper); err != nil {
		return err
	}
	cmd := exec.Command("systemd-run", "--unit=jump-agent-update-"+operation,
		"--collect", "--property=Type=exec", helper, "internal-update-helper", operation, checksum)
	if err := cmd.Run(); err != nil {
		os.Remove(helper)
		return err
	}
	return nil
}

func copyCurrentTo(path string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0700)
}

func updateHelper(args []string) error {
	if len(args) != 2 || !validOperation(args[0]) || os.Geteuid() != 0 {
		return errors.New("invalid helper invocation")
	}
	helper := linuxInstallPath + ".helper-" + args[0]
	staged := linuxInstallPath + ".update-" + args[0]
	backup := linuxInstallPath + ".backup-" + args[0]
	defer os.Remove(helper)
	defer os.Remove(staged)
	if self, err := os.Executable(); err != nil || filepath.Clean(self) != helper {
		return errors.New("invalid helper location")
	}
	if err := verifyStaged(staged, args[1]); err != nil {
		return err
	}
	if err := runSystemctl("stop", linuxServiceName); err != nil {
		return err
	}
	return replaceLinuxBinary(linuxInstallPath, staged, backup,
		func() error { return runSystemctl("start", linuxServiceName) },
		func() error { return runSystemctl("stop", linuxServiceName) },
		func() bool {
			time.Sleep(3 * time.Second)
			output, err := exec.Command("systemctl", "is-active", linuxServiceName).Output()
			return err == nil && strings.TrimSpace(string(output)) == "active"
		})
}

func replaceLinuxBinary(target, staged, backup string, start, stop func() error, ready func() bool) error {
	// Preserve the known-good inode and atomically rename the verified binary.
	if err := os.Link(target, backup); err != nil {
		_ = start()
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		os.Remove(backup)
		_ = start()
		return err
	}
	if err := start(); err == nil && ready() {
		return os.Remove(backup)
	}
	if err := stop(); err != nil {
		return err
	}
	if err := os.Rename(backup, target); err != nil {
		return err
	}
	return start()
}
