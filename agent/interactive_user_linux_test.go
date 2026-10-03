//go:build linux

package main

import (
	"errors"
	"testing"
)

func TestLocalSessionUser(t *testing.T) {
	for _, test := range []struct{ name, props, want string }{
		{"graphical", "Name=jared\nActive=yes\nRemote=no\nType=wayland\nClass=user", "jared"},
		{"console", "Name=root\nActive=yes\nRemote=no\nType=tty\nClass=user", "root"},
		{"ssh", "Name=root\nActive=yes\nRemote=yes\nType=tty\nClass=user", ""},
		{"inactive", "Name=jared\nActive=no\nRemote=no\nType=x11\nClass=user", ""},
		{"greeter", "Name=gdm\nActive=yes\nRemote=no\nType=wayland\nClass=greeter", ""},
		{"daemon", "Name=root\nActive=yes\nRemote=no\nType=unspecified\nClass=manager", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := func(_ string, args ...string) ([]byte, error) {
				if args[0] == "list-sessions" {
					return []byte("1 1000 jared seat0 tty1\n"), nil
				}
				return []byte(test.props), nil
			}
			if got := localSessionUser(run); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
	if got := localSessionUser(func(_ string, _ ...string) ([]byte, error) { return nil, nil }); got != "" {
		t.Fatalf("headless host reported %q", got)
	}
}

func TestLocalSessionFallback(t *testing.T) {
	for _, test := range []struct{ who, want string }{
		{"root pts/0 2026-10-03 10:00 (192.0.2.1)\n", ""},
		{"jared tty1 2026-10-03 10:00\n", "jared"},
		{"jared console 2026-10-03 10:00\n", "jared"},
		{"", ""},
	} {
		run := func(name string, _ ...string) ([]byte, error) {
			if name == "loginctl" {
				return nil, errors.New("not installed")
			}
			return []byte(test.who), nil
		}
		if got := localSessionUser(run); got != test.want {
			t.Fatalf("got %q, want %q", got, test.want)
		}
	}
}
