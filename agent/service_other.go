//go:build !linux && !windows

package main

import "fmt"

func installService() error {
	return fmt.Errorf("native service installation is unsupported on this platform")
}
func uninstallService() error {
	return fmt.Errorf("native service installation is unsupported on this platform")
}
func startService() error {
	return fmt.Errorf("native service installation is unsupported on this platform")
}
func stopService() error {
	return fmt.Errorf("native service installation is unsupported on this platform")
}
func runAgentCommand() error { return runForeground() }
