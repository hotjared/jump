//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
)

func runSystemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s failed: %w: %s", args[0], err, string(output))
	}
	return nil
}

func installService() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("service install requires root; run with sudo")
	}
	if err := copyExecutable(linuxInstallPath); err != nil {
		return err
	}
	if err := os.WriteFile(linuxUnitPath, []byte(linuxSystemdUnit()), 0644); err != nil {
		return fmt.Errorf("write systemd unit: %w", err)
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", linuxServiceName); err != nil {
		return err
	}
	return nil
}

func uninstallService() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("service uninstall requires root; run with sudo")
	}
	_ = runSystemctl("disable", "--now", linuxServiceName)
	if err := os.Remove(linuxUnitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove systemd unit: %w", err)
	}
	return runSystemctl("daemon-reload")
}

func startService() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("service start requires root; run with sudo")
	}
	return runSystemctl("start", linuxServiceName)
}

func stopService() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("service stop requires root; run with sudo")
	}
	return runSystemctl("stop", linuxServiceName)
}
