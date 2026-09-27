//go:build linux

package main

import "syscall"

func safeReadFlag() int { return syscall.O_NONBLOCK }

func localDrive(string) bool   { return false }
func localDrives() []fileEntry { return nil }
