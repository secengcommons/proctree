//go:build linux

package proctree

import (
	"errors"
	"io"
	"os"
	"strconv"
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
	root, err := os.OpenRoot("/proc")
	if err != nil {
		return true, err
	}
	state, _, readErr := readLinuxProcessStat(root, strconv.Itoa(processID)+"/stat", func(root *os.Root, path string) (io.ReadCloser, error) {
		return root.Open(path)
	})
	rootCloseErr := root.Close()
	if errors.Is(readErr, os.ErrNotExist) {
		return false, rootCloseErr
	}
	if readErr != nil || rootCloseErr != nil {
		return true, errors.Join(readErr, rootCloseErr)
	}
	return liveLinuxProcessState(state), nil
}
