package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceCommandRequiresEnrollment(t *testing.T) {
	t.Setenv("JUMP_AGENT_STATE", filepath.Join(t.TempDir(), "missing.json"))
	if err := serviceCommand([]string{"install"}); err == nil || !strings.Contains(err.Error(), "not enrolled") {
		t.Fatalf("service install without identity = %v", err)
	}
}

func TestServiceCommandParsing(t *testing.T) {
	for _, args := range [][]string{{}, {"install", "extra"}, {"unknown"}} {
		if err := serviceCommand(args); err == nil {
			t.Fatalf("serviceCommand(%v) unexpectedly succeeded", args)
		}
	}
}

func TestLinuxSystemdUnit(t *testing.T) {
	unit := linuxSystemdUnit()
	for _, want := range []string{
		"ExecStart=/usr/local/bin/jump-agent run",
		"Restart=on-failure",
		"WantedBy=multi-user.target",
		"UMask=0077",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("systemd unit missing %q:\n%s", want, unit)
		}
	}
}

func TestWindowsServiceConfiguration(t *testing.T) {
	commands := windowsInstallCommands("C:\\Program Files\\Jump\\jump-agent.exe")
	joined := make([]string, 0, len(commands))
	for _, command := range commands {
		joined = append(joined, strings.Join(command, " "))
	}
	all := strings.Join(joined, "\n")
	for _, want := range []string{
		"create JumpAgent",
		"service run",
		"start= auto",
		"obj= LocalSystem",
		"DisplayName= Jump Agent",
		"restart/5000",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("Windows service config missing %q:\n%s", want, all)
		}
	}
}
