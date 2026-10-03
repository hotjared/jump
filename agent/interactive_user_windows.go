//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var querySessionUser = windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSQuerySessionInformationW")

func consoleSessionText(session, class uint32) string {
	var buffer *uint16
	var size uint32
	result, _, _ := querySessionUser.Call(0, uintptr(session), uintptr(class), uintptr(unsafe.Pointer(&buffer)), uintptr(unsafe.Pointer(&size)))
	if result == 0 || buffer == nil {
		return ""
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(buffer)))
	if size < 2 || size > 65536 || size%2 != 0 {
		return ""
	}
	return windows.UTF16ToString(unsafe.Slice(buffer, size/2))
}

func interactiveUser() string {
	return consoleInteractiveUser(windows.WTSGetActiveConsoleSessionId(), consoleSessionText)
}

func consoleInteractiveUser(session uint32, query func(uint32, uint32) string) string {
	if session == 0xffffffff {
		return ""
	}
	// Query the console, including a signed-in user's locked desktop. Never
	// derive this from the LocalSystem service token or machine account.
	return interactiveAccount(query(session, 7), query(session, 5))
}
