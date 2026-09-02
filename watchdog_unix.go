//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package proctree

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const watchdogArgument = "__proctree-watchdog"
const watchdogControlDescriptor = 3
const watchdogReadyDescriptor = 4
const watchdogDrainBufferBytes = 1024
const watchdogReadySignal = byte('R')

type watchdogAPI struct {
	file  func(uintptr, string) *os.File
	pid   func() int
	group func() (int, error)
	kill  func(int, syscall.Signal) error
	drain func(*os.File) error
	ready func(*os.File) error
}

func systemWatchdogAPI() watchdogAPI {
	return watchdogAPI{
		file: os.NewFile, pid: os.Getpid, group: func() (int, error) { return unix.Getpgid(0) },
		kill: unix.Kill, drain: drainWatchdog,
		ready: signalWatchdogReady,
	}
}

func DispatchWatchdog(arguments []string) (bool, int) {
	return dispatchWatchdog(arguments, systemWatchdogAPI())
}

func dispatchWatchdog(arguments []string, api watchdogAPI) (bool, int) {
	if len(arguments) != 2 || arguments[1] != watchdogArgument {
		return false, 0
	}
	if err := runWatchdog(api); err != nil {
		return true, 1
	}
	return true, 0
}

func runWatchdog(api watchdogAPI) error {
	control := api.file(watchdogControlDescriptor, "proctree-controller")
	if control == nil {
		return errors.New("watchdog control descriptor is unavailable")
	}
	ready := api.file(watchdogReadyDescriptor, "proctree-ready")
	if ready == nil {
		return errors.Join(errors.New("watchdog readiness descriptor is unavailable"), control.Close())
	}
	if err := validateWatchdogPipe(control, "control"); err != nil {
		return errors.Join(err, control.Close(), ready.Close())
	}
	if err := validateWatchdogPipe(ready, "readiness"); err != nil {
		return errors.Join(err, control.Close(), ready.Close())
	}
	group, err := api.group()
	if err != nil {
		return errors.Join(err, control.Close(), ready.Close())
	}
	if pid := api.pid(); pid <= 0 || group <= 0 || pid != group {
		return errors.Join(errors.New("watchdog is not its process-group leader"), control.Close(), ready.Close())
	}
	if err = api.ready(ready); err != nil {
		return errors.Join(err, control.Close())
	}
	if err = api.drain(control); err != nil {
		return err
	}
	return api.kill(-group, syscall.SIGKILL)
}

func validateWatchdogPipe(file *os.File, name string) error {
	information, err := file.Stat()
	if err != nil {
		return err
	}
	if information.Mode()&os.ModeNamedPipe == 0 {
		return fmt.Errorf("watchdog %s descriptor is not a pipe", name)
	}
	return nil
}

func signalWatchdogReady(file *os.File) error {
	_, writeErr := file.Write([]byte{watchdogReadySignal})
	return errors.Join(writeErr, file.Close())
}

func drainWatchdog(control *os.File) error {
	var buffer [watchdogDrainBufferBytes]byte
	for {
		_, err := control.Read(buffer[:])
		if errors.Is(err, io.EOF) {
			return control.Close()
		}
		if err != nil {
			return errors.Join(err, control.Close())
		}
	}
}
