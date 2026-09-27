//go:build windows

package main

import (
	"golang.org/x/sys/windows"
	"strings"
)

func safeReadFlag() int { return 0 }

func localDrive(path string) bool {
	if len(path) != 3 || path[1] != ':' || path[2] != '\\' {
		return false
	}
	mask, e := windows.GetLogicalDrives()
	if e != nil {
		return false
	}
	bit := uint32(1) << uint32(strings.ToUpper(path[:1])[0]-'A')
	return mask&bit != 0 && windows.GetDriveType(windows.StringToUTF16Ptr(path)) == windows.DRIVE_FIXED
}
func localDrives() []fileEntry {
	entries := []fileEntry{}
	for c := 'A'; c <= 'Z'; c++ {
		drive := string(c) + ":\\"
		if localDrive(drive) {
			entries = append(entries, fileEntry{Name: drive, Type: "directory"})
		}
	}
	return entries
}
