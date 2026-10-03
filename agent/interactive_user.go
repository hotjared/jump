package main

import "strings"

func interactiveAccount(domain, username string) string {
	username = strings.TrimSpace(username)
	if username == "" || strings.HasSuffix(username, "$") {
		return ""
	}
	switch strings.ToUpper(username) {
	case "SYSTEM", "LOCAL SYSTEM", "LOCALSYSTEM", "LOCAL SERVICE", "NETWORK SERVICE":
		return ""
	}
	if domain = strings.TrimSpace(domain); domain != "" {
		return domain + `\` + username
	}
	return username
}
