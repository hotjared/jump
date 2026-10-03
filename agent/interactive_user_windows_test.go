//go:build windows

package main

import "testing"

func TestConsoleInteractiveUser(t *testing.T) {
	for _, test := range []struct{ domain, user, want string }{
		{"AD", "Jared", `AD\Jared`}, {"", "HJ Admin", "HJ Admin"},
		{"AD", "PROD-DC01$", ""}, {"NT AUTHORITY", "SYSTEM", ""}, {"", "", ""},
	} {
		got := consoleInteractiveUser(3, func(session, class uint32) string {
			if session != 3 {
				t.Fatal("queried service session instead of the console")
			}
			if class == 7 {
				return test.domain
			}
			return test.user
		})
		if got != test.want {
			t.Errorf("got %q, want %q", got, test.want)
		}
	}
	if got := consoleInteractiveUser(0xffffffff, func(_, _ uint32) string {
		t.Fatal("queried absent console session")
		return ""
	}); got != "" {
		t.Fatalf("no console reported %q", got)
	}
}
