package main

import "testing"

func TestInteractiveAccount(t *testing.T) {
	for _, test := range []struct{ domain, user, want string }{
		{"AD", "Jared", `AD\Jared`}, {"", "HJ Admin", "HJ Admin"},
		{"AD", "PROD-DC01$", ""}, {"NT AUTHORITY", "SYSTEM", ""},
		{"", "LocalSystem", ""}, {"", "NETWORK SERVICE", ""}, {"AD", "", ""},
	} {
		if got := interactiveAccount(test.domain, test.user); got != test.want {
			t.Errorf("interactiveAccount(%q, %q) = %q, want %q", test.domain, test.user, got, test.want)
		}
	}
}
