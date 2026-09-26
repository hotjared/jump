//go:build !linux && !windows

package main

import "errors"

func installedAgentPath() string              { return "" }
func launchUpdateHelper(string, string) error { return errors.New("unsupported platform") }
func updateHelper([]string) error             { return errors.New("unsupported platform") }
