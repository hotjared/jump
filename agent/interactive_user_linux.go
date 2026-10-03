//go:build linux

package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

type sessionCommand func(string, ...string) ([]byte, error)

func localSessionUser(run sessionCommand) string {
	if output, err := run("loginctl", "list-sessions", "--no-legend", "--no-pager"); err == nil {
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			data, err := run("loginctl", "show-session", fields[0], "--no-pager", "-p", "Name", "-p", "Active", "-p", "Remote", "-p", "Type", "-p", "Class")
			if err != nil {
				continue
			}
			props := map[string]string{}
			for _, property := range strings.Split(string(data), "\n") {
				if key, value, found := strings.Cut(property, "="); found {
					props[key] = value
				}
			}
			if props["Active"] == "yes" && props["Remote"] == "no" && (props["Class"] == "user" || props["Class"] == "user-early") && (props["Type"] == "tty" || props["Type"] == "x11" || props["Type"] == "wayland") {
				return interactiveAccount("", props["Name"])
			}
		}
		return ""
	}
	// Non-systemd hosts: accept actual local tty/console logins, not SSH pts
	// sessions or the account running the daemon.
	if output, err := run("who"); err == nil {
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			tty := fields[1]
			localTTY := strings.HasPrefix(tty, "tty") && len(tty) > 3 && strings.Trim(tty[3:], "0123456789") == ""
			if tty == "console" || localTTY {
				return interactiveAccount("", fields[0])
			}
		}
	}
	return ""
}

func interactiveUser() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return localSessionUser(func(name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	})
}
