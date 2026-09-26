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

var (
	linuxEffectiveUID   = os.Geteuid
	linuxCopyExecutable = copyExecutable
	linuxWriteFile      = os.WriteFile
	linuxRemoveFile     = os.Remove
	linuxSystemctl      = runSystemctl
	linuxBinaryTarget   = linuxInstallPath
	linuxUnitTarget     = linuxUnitPath
)

func installService() error {
	if linuxEffectiveUID() != 0 {
		return fmt.Errorf("service install requires root; run with sudo")
	}
	if err := linuxCopyExecutable(linuxBinaryTarget); err != nil {
		return err
	}
	if err := linuxWriteFile(linuxUnitTarget, []byte(linuxSystemdUnit()), 0644); err != nil {
		return fmt.Errorf("write systemd unit: %w", err)
	}
	if err := linuxSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := linuxSystemctl("enable", "--now", linuxServiceName); err != nil {
		return err
	}
	return nil
}

func uninstallService() error {
	if linuxEffectiveUID() != 0 {
		return fmt.Errorf("service uninstall requires root; run with sudo")
	}
	_ = linuxSystemctl("disable", "--now", linuxServiceName)
	if err := linuxRemoveFile(linuxUnitTarget); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove systemd unit: %w", err)
	}
	return linuxSystemctl("daemon-reload")
}

func startService() error {
	if linuxEffectiveUID() != 0 {
		return fmt.Errorf("service start requires root; run with sudo")
	}
	return linuxSystemctl("start", linuxServiceName)
}

func stopService() error {
	if linuxEffectiveUID() != 0 {
		return fmt.Errorf("service stop requires root; run with sudo")
	}
	return linuxSystemctl("stop", linuxServiceName)
}

func runAgentCommand() error {
	return runForeground()
}
