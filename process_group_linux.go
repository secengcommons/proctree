//go:build linux

package proctree

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const linuxProcessBatch = 128
const maxLinuxProcessStatBytes = 1 << 12
const minimumLinuxProcessStatFields = 3
const linuxProcessStateField = 0
const linuxProcessGroupField = 2

type linuxProcessDirectory interface {
	ReadDir(int) ([]os.DirEntry, error)
	Close() error
}

type linuxProcessGroupAPI struct {
	openRoot      func(string) (*os.Root, error)
	openDirectory func(string) (linuxProcessDirectory, error)
	openStat      func(*os.Root, string) (io.ReadCloser, error)
	now           func() time.Time
}

func processGroupTerminated(group int, deadline time.Time) (bool, error) {
	return processGroupTerminatedWith(group, deadline, "/proc", systemLinuxProcessGroupAPI())
}

func systemLinuxProcessGroupAPI() linuxProcessGroupAPI {
	return linuxProcessGroupAPI{
		openRoot: os.OpenRoot,
		openDirectory: func(path string) (linuxProcessDirectory, error) {
			return os.Open(path)
		},
		openStat: func(root *os.Root, path string) (io.ReadCloser, error) {
			return root.Open(path)
		},
		now: time.Now,
	}
}

func processGroupTerminatedWith(group int, deadline time.Time, path string, api linuxProcessGroupAPI) (bool, error) {
	if !api.now().Before(deadline) {
		return false, nil
	}
	root, err := api.openRoot(path)
	if err != nil {
		return false, err
	}
	directory, err := api.openDirectory(path)
	if err != nil {
		return false, errors.Join(err, root.Close())
	}
	terminated, scanErr := scanLinuxProcessGroup(group, deadline, root, directory, api)
	return terminated, errors.Join(scanErr, directory.Close(), root.Close())
}

func scanLinuxProcessGroup(
	group int,
	deadline time.Time,
	root *os.Root,
	directory linuxProcessDirectory,
	api linuxProcessGroupAPI,
) (bool, error) {
	for {
		if !api.now().Before(deadline) {
			return false, nil
		}
		entries, err := directory.ReadDir(linuxProcessBatch)
		for _, entry := range entries {
			if !api.now().Before(deadline) {
				return false, nil
			}
			live, entryErr := liveLinuxProcessGroupEntry(group, root, entry, api.openStat)
			if entryErr != nil {
				return false, entryErr
			}
			if live {
				return false, nil
			}
		}
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
}

func liveLinuxProcessGroupEntry(
	group int,
	root *os.Root,
	entry os.DirEntry,
	openStat func(*os.Root, string) (io.ReadCloser, error),
) (bool, error) {
	name := entry.Name()
	if !entry.IsDir() || !decimalProcessID(name) {
		return false, nil
	}
	state, processGroup, err := readLinuxProcessStat(root, filepath.Join(name, "stat"), openStat)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return processGroup == group && liveLinuxProcessState(state), nil
}

func liveLinuxProcessState(state byte) bool {
	return state != 'Z' && state != 'X' && state != 'x'
}

func decimalProcessID(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func readLinuxProcessStat(
	root *os.Root,
	path string,
	open func(*os.Root, string) (io.ReadCloser, error),
) (byte, int, error) {
	file, err := open(root, path)
	if err != nil {
		return 0, 0, err
	}
	var buffer [maxLinuxProcessStatBytes + 1]byte
	read, readErr := io.ReadFull(file, buffer[:])
	closeErr := file.Close()
	if readErr == nil {
		return 0, 0, errors.Join(errors.New("Linux process status exceeds its bound"), closeErr)
	}
	if !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return 0, 0, errors.Join(readErr, closeErr)
	}
	state, group, valid := parseLinuxProcessStat(buffer[:read])
	if !valid {
		return 0, 0, errors.Join(errors.New("invalid Linux process status"), closeErr)
	}
	return state, group, closeErr
}

func parseLinuxProcessStat(source []byte) (byte, int, bool) {
	end := bytes.LastIndexByte(source, ')')
	if end < 0 {
		return 0, 0, false
	}
	fields := bytes.Fields(source[end+1:])
	if len(fields) < minimumLinuxProcessStatFields || len(fields[linuxProcessStateField]) != 1 {
		return 0, 0, false
	}
	group, err := strconv.Atoi(string(fields[linuxProcessGroupField]))
	if err != nil || group < 0 {
		return 0, 0, false
	}
	state := fields[linuxProcessStateField][0]
	switch state {
	case 'R', 'S', 'D', 'Z', 'T', 't', 'X', 'x', 'K', 'W', 'P', 'I':
	default:
		return 0, 0, false
	}
	return state, group, true
}
