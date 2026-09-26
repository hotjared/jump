//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func installedAgentPath() string { return windowsInstallPath() }

func launchUpdateHelper(operation, checksum string) error {
	helper := installedAgentPath() + ".helper-" + operation + ".exe"
	self, err := os.Executable()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	if err := os.WriteFile(helper, data, 0700); err != nil {
		return err
	}
	cmd := exec.Command(helper, "internal-update-helper", operation, checksum)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x00000008}
	if err := cmd.Start(); err != nil {
		os.Remove(helper)
		return err
	}
	return cmd.Process.Release()
}

func serviceState() string {
	output, err := exec.Command("sc.exe", "query", windowsServiceName).Output()
	if err != nil {
		return ""
	}
	if strings.Contains(string(output), "RUNNING") {
		return "RUNNING"
	}
	if strings.Contains(string(output), "STOPPED") {
		return "STOPPED"
	}
	return "PENDING"
}

func waitService(expected string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if serviceState() == expected {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func updateHelper(args []string) error {
	if len(args) != 2 || !validOperation(args[0]) {
		return errors.New("invalid helper invocation")
	}
	target := installedAgentPath()
	helper := target + ".helper-" + args[0] + ".exe"
	staged := target + ".update-" + args[0]
	backup := target + ".backup-" + args[0]
	if self, err := os.Executable(); err != nil || !strings.EqualFold(filepath.Clean(self), filepath.Clean(helper)) {
		return errors.New("invalid helper location")
	}
	defer scheduleDelete(helper)
	defer os.Remove(staged)
	if err := verifyStaged(staged, args[1]); err != nil {
		return err
	}
	_ = runSC("stop", windowsServiceName)
	if !waitService("STOPPED", 25*time.Second) {
		return errors.New("service did not stop")
	}
	return replaceStoppedBinary(target, staged, backup,
		func() error { return runSC("start", windowsServiceName) },
		func() bool { return waitService("RUNNING", 15*time.Second) },
		func() error {
			_ = runSC("stop", windowsServiceName)
			if !waitService("STOPPED", 20*time.Second) {
				return errors.New("updated service could not stop for rollback")
			}
			return nil
		})
}

func scheduleDelete(path string) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")
	_, _, _ = proc.Call(uintptr(unsafe.Pointer(name)), 0, 4) // MOVEFILE_DELAY_UNTIL_REBOOT
}
