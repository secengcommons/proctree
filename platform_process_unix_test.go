//go:build darwin || dragonfly || freebsd || illumos || netbsd || openbsd || solaris

package proctree

import (
	"errors"
	"syscall"
)

func testProcessAlive(processID int) (bool, error) {
	err := syscall.Kill(processID, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false, err
	}
	return true, nil
}
