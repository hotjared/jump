//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	serviceWin32OwnProcess = 0x00000010
	serviceStopped         = 0x00000001
	serviceStartPending    = 0x00000002
	serviceStopPending     = 0x00000003
	serviceRunning         = 0x00000004
	serviceAcceptStop      = 0x00000001
	serviceAcceptShutdown  = 0x00000004
	serviceControlStop     = 0x00000001
	serviceControlShutdown = 0x00000005
)

var (
	advapi32                         = syscall.NewLazyDLL("advapi32.dll")
	procStartServiceCtrlDispatcherW  = advapi32.NewProc("StartServiceCtrlDispatcherW")
	procRegisterServiceCtrlHandlerEx = advapi32.NewProc("RegisterServiceCtrlHandlerExW")
	procSetServiceStatus             = advapi32.NewProc("SetServiceStatus")
	serviceCancel                    context.CancelFunc
)

type serviceTableEntry struct {
	serviceName *uint16
	serviceProc uintptr
}

type serviceStatus struct {
	serviceType             uint32
	currentState            uint32
	controlsAccepted        uint32
	win32ExitCode           uint32
	serviceSpecificExitCode uint32
	checkPoint              uint32
	waitHint                uint32
}

func windowsInstallPath() string {
	base := os.Getenv("ProgramFiles")
	if base == "" {
		base = "C:\\Program Files"
	}
	return filepath.Join(base, "Jump", "jump-agent.exe")
}

func runSC(args ...string) error {
	cmd := exec.Command("sc.exe", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sc.exe %s failed: %w: %s", args[0], err, string(output))
	}
	return nil
}

func installService() error {
	target := windowsInstallPath()
	if err := copyExecutable(target); err != nil {
		return err
	}
	for _, args := range windowsInstallCommands(target) {
		if err := runSC(args...); err != nil {
			return err
		}
	}
	return runSC("start", windowsServiceName)
}

func uninstallService() error {
	_ = runSC("stop", windowsServiceName)
	return runSC("delete", windowsServiceName)
}

func startService() error {
	return runSC("start", windowsServiceName)
}

func stopService() error {
	return runSC("stop", windowsServiceName)
}

func setWindowsServiceStatus(handle uintptr, state, accepted uint32) error {
	status := serviceStatus{
		serviceType:      serviceWin32OwnProcess,
		currentState:     state,
		controlsAccepted: accepted,
	}
	r1, _, err := procSetServiceStatus.Call(handle, uintptr(unsafe.Pointer(&status)))
	if r1 == 0 {
		return err
	}
	return nil
}

func windowsServiceControl(control, eventType, eventData, callbackContext uintptr) uintptr {
	switch uint32(control) {
	case serviceControlStop, serviceControlShutdown:
		if serviceCancel != nil {
			serviceCancel()
		}
	}
	return 0
}

func windowsServiceMain(argc, argv uintptr) uintptr {
	name, _ := syscall.UTF16PtrFromString(windowsServiceName)
	handler := syscall.NewCallback(windowsServiceControl)
	handle, _, _ := procRegisterServiceCtrlHandlerEx.Call(
		uintptr(unsafe.Pointer(name)), handler, 0,
	)
	if handle == 0 {
		return 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	serviceCancel = cancel
	defer func() {
		serviceCancel = nil
		cancel()
		_ = setWindowsServiceStatus(handle, serviceStopped, 0)
	}()
	_ = setWindowsServiceStatus(handle, serviceStartPending, 0)
	_ = setWindowsServiceStatus(handle, serviceRunning, serviceAcceptStop|serviceAcceptShutdown)
	_ = run(ctx)
	_ = setWindowsServiceStatus(handle, serviceStopPending, 0)
	return 0
}

func runWindowsService() error {
	name, err := syscall.UTF16PtrFromString(windowsServiceName)
	if err != nil {
		return err
	}
	table := []serviceTableEntry{
		{serviceName: name, serviceProc: syscall.NewCallback(windowsServiceMain)},
		{},
	}
	r1, _, callErr := procStartServiceCtrlDispatcherW.Call(uintptr(unsafe.Pointer(&table[0])))
	if r1 == 0 {
		return fmt.Errorf("start Windows service dispatcher: %w", callErr)
	}
	return nil
}

func runAgentCommand() error {
	err := runWindowsService()
	if errors.Is(err, syscall.Errno(1063)) {
		return runForeground()
	}
	return err
}
