package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	windowsServiceName        = "JumpAgent"
	windowsServiceDisplayName = "Jump Agent"
	linuxServiceName          = "jump-agent"
	linuxInstallPath          = "/usr/local/bin/jump-agent"
	linuxUnitPath             = "/etc/systemd/system/jump-agent.service"
)

func serviceCommand(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: jump-agent service install|uninstall|start|stop")
	}
	switch args[0] {
	case "install":
		if err := requireIdentity(); err != nil {
			return err
		}
		return installService()
	case "uninstall":
		return uninstallService()
	case "start":
		return startService()
	case "stop":
		return stopService()
	default:
		return fmt.Errorf("unknown service command %q", args[0])
	}
}

func requireIdentity() error {
	info, err := os.Stat(statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("agent is not enrolled: identity not found at %s", statePath())
		}
		return fmt.Errorf("check agent identity: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("agent identity path is a directory: %s", statePath())
	}
	return nil
}

func copyExecutable(target string) error {
	source, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate agent executable: %w", err)
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	if strings.EqualFold(source, target) {
		return nil
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read agent executable: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return fmt.Errorf("create agent install directory: %w", err)
	}
	temp := target + ".tmp"
	if err := os.WriteFile(temp, data, 0755); err != nil {
		return fmt.Errorf("write agent executable: %w", err)
	}
	if err := os.Chmod(temp, 0755); err != nil {
		os.Remove(temp)
		return fmt.Errorf("set agent executable permissions: %w", err)
	}
	if err := os.Rename(temp, target); err != nil {
		os.Remove(temp)
		return fmt.Errorf("install agent executable: %w", err)
	}
	return nil
}

func linuxSystemdUnit() string {
	return "[Unit]\n" +
		"Description=Jump Agent\n" +
		"After=network-online.target\n" +
		"Wants=network-online.target\n\n" +
		"[Service]\n" +
		"Type=simple\n" +
		"ExecStart=/usr/local/bin/jump-agent run\n" +
		"Restart=on-failure\n" +
		"RestartSec=5s\n" +
		"UMask=0077\n\n" +
		"[Install]\n" +
		"WantedBy=multi-user.target\n"
}

func windowsInstallCommands(executable string) [][]string {
	binPath := fmt.Sprintf("%q run", executable)
	return [][]string{
		{"create", windowsServiceName, "binPath=", binPath, "start=", "auto", "obj=", "LocalSystem", "DisplayName=", windowsServiceDisplayName},
		{"failure", windowsServiceName, "reset=", "86400", "actions=", "restart/5000/restart/5000/restart/5000"},
		{"failureflag", windowsServiceName, "1"},
	}
}
