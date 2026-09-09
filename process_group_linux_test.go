//go:build linux

package proctree

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSystemProcessGroupIsLive(t *testing.T) {
	group, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatal(err)
	}
	if terminated, inspectErr := processGroupTerminated(group, linuxProcessDeadline()); inspectErr != nil || terminated {
		t.Fatalf("current group = (%t, %v)", terminated, inspectErr)
	}
}

func TestProcessGroupTerminated(t *testing.T) {
	root := t.TempDir()
	writeProcessStat(t, root, "11", "11 (zombie) Z 1 41 0 0\n")
	writeProcessStat(t, root, "12", "12 (other) R 1 42 0 0\n")
	if terminated, err := processGroupTerminatedWith(41, linuxProcessDeadline(), root, systemLinuxProcessGroupAPI()); err != nil || !terminated {
		t.Fatalf("zombie group = (%t, %v)", terminated, err)
	}
	writeProcessStat(t, root, "13", "13 (live) S 1 41 0 0\n")
	if terminated, err := processGroupTerminatedWith(41, linuxProcessDeadline(), root, systemLinuxProcessGroupAPI()); err != nil || terminated {
		t.Fatalf("live group = (%t, %v)", terminated, err)
	}
}

func TestProcessGroupInspectionFailures(t *testing.T) {
	failure := errors.New("inspection")
	api := systemLinuxProcessGroupAPI()
	api.openRoot = func(string) (*os.Root, error) { return nil, failure }
	if _, err := processGroupTerminatedWith(41, linuxProcessDeadline(), "unused", api); !errors.Is(err, failure) {
		t.Fatalf("root error = %v", err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "invalid"), 0o700); err != nil {
		t.Fatal(err)
	}
	api = systemLinuxProcessGroupAPI()
	api.openDirectory = func(*os.Root) (linuxProcessDirectory, error) { return nil, failure }
	if _, err := processGroupTerminatedWith(41, linuxProcessDeadline(), root, api); !errors.Is(err, failure) {
		t.Fatalf("directory error = %v", err)
	}
	writeProcessStat(t, root, "11", "11 (process) S 1 41 0 0\n")
	api = systemLinuxProcessGroupAPI()
	api.openStat = func(*os.Root, string) (io.ReadCloser, error) { return nil, failure }
	if _, err := processGroupTerminatedWith(41, linuxProcessDeadline(), root, api); !errors.Is(err, failure) {
		t.Fatalf("status open error = %v", err)
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = scanLinuxProcessGroup(41, linuxProcessDeadline(), opened, &failedProcessDirectory{err: failure}, api); !errors.Is(err, failure) {
		t.Fatalf("directory read error = %v", err)
	}
	if closeErr := opened.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	api = systemLinuxProcessGroupAPI()
	api.openDirectory = func(*os.Root) (linuxProcessDirectory, error) {
		return &failedProcessDirectory{err: io.EOF, closeErr: failure}, nil
	}
	if _, err = processGroupTerminatedWith(41, linuxProcessDeadline(), root, api); !errors.Is(err, failure) {
		t.Fatalf("directory close error = %v", err)
	}
}

func TestReadLinuxProcessStat(t *testing.T) {
	root := t.TempDir()
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := opened.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	for _, test := range []struct {
		name   string
		source string
		valid  bool
	}{
		{name: "live", source: "12 (name with ) mark) R 1 41 0 0\n", valid: true},
		{name: "missing close", source: "12 name R 1 41", valid: false},
		{name: "fields", source: "12 (name) R 1", valid: false},
		{name: "state", source: "12 (name) ? 1 41", valid: false},
		{name: "group", source: "12 (name) R 1 invalid", valid: false},
		{name: "zero group", source: "12 (name) R 1 0", valid: true},
		{name: "negative group", source: "12 (name) R 1 -1", valid: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := test.name + ".stat"
			if err := os.WriteFile(filepath.Join(root, path), []byte(test.source), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, readErr := readLinuxProcessStat(opened, path, func(root *os.Root, path string) (io.ReadCloser, error) {
				return root.Open(path)
			})
			if (readErr == nil) != test.valid {
				t.Fatalf("error = %v", readErr)
			}
		})
	}
	tooLarge := strings.Repeat("x", maxLinuxProcessStatBytes+1)
	if err := os.WriteFile(filepath.Join(root, "large.stat"), []byte(tooLarge), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readLinuxProcessStat(opened, "large.stat", func(root *os.Root, path string) (io.ReadCloser, error) {
		return root.Open(path)
	}); err == nil {
		t.Fatal("oversized status accepted")
	}
	if _, _, err := readLinuxProcessStat(opened, "missing.stat", func(root *os.Root, path string) (io.ReadCloser, error) {
		return root.Open(path)
	}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing status error = %v", err)
	}
	failure := errors.New("read")
	if _, _, err := readLinuxProcessStat(opened, "unused", func(*os.Root, string) (io.ReadCloser, error) {
		return &failedProcessStat{readErr: failure}, nil
	}); !errors.Is(err, failure) {
		t.Fatalf("status read error = %v", err)
	}
	if _, _, err := readLinuxProcessStat(opened, "unused", func(*os.Root, string) (io.ReadCloser, error) {
		return &failedProcessStat{source: []byte("12 (name) R 1 41"), closeErr: failure}, nil
	}); !errors.Is(err, failure) {
		t.Fatalf("status close error = %v", err)
	}
}

func TestScanLinuxProcessGroupSkipsVanishedAndNonProcessEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "not-a-process"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "12"), 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := opened.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	api := systemLinuxProcessGroupAPI()
	api.openStat = func(*os.Root, string) (io.ReadCloser, error) {
		return nil, os.ErrNotExist
	}
	terminated, scanErr := scanLinuxProcessGroup(41, linuxProcessDeadline(), opened, directory, api)
	if closeErr := errors.Join(directory.Close(), opened.Close()); closeErr != nil {
		t.Fatal(closeErr)
	}
	if scanErr != nil || !terminated {
		t.Fatalf("scan = (%t, %v)", terminated, scanErr)
	}
}

func TestLinuxProcessIdentityParsing(t *testing.T) {
	for value, valid := range map[string]bool{"1": true, "": false, "1a": false} {
		if decimalProcessID(value) != valid {
			t.Fatalf("decimalProcessID(%q) = %t", value, !valid)
		}
	}
}

func TestLinuxProcessGroupInspectionHonoursDeadline(t *testing.T) {
	deadline := time.Unix(1, 0)
	api := systemLinuxProcessGroupAPI()
	api.now = func() time.Time { return deadline }
	if terminated, err := processGroupTerminatedWith(41, deadline, "unused", api); err != nil || terminated {
		t.Fatalf("expired inspection = (%t, %v)", terminated, err)
	}
	if terminated, err := scanLinuxProcessGroup(41, deadline, nil, &failedProcessDirectory{err: errors.New("unused")}, api); err != nil || terminated {
		t.Fatalf("expired scan = (%t, %v)", terminated, err)
	}
	root := t.TempDir()
	writeProcessStat(t, root, "11", "11 (process) S 1 41 0 0\n")
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := opened.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	api.now = func() time.Time {
		calls++
		if calls == 1 {
			return deadline.Add(-time.Second)
		}
		return deadline
	}
	if terminated, scanErr := scanLinuxProcessGroup(41, deadline, opened, directory, api); scanErr != nil || terminated {
		t.Fatalf("scan deadline = (%t, %v)", terminated, scanErr)
	}
	if closeErr := errors.Join(directory.Close(), opened.Close()); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func linuxProcessDeadline() time.Time { return time.Now().Add(time.Minute) }

func writeProcessStat(t *testing.T, root, processID, source string) {
	t.Helper()
	directory := filepath.Join(root, processID)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "stat"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

type failedProcessDirectory struct {
	err      error
	closeErr error
}

func (directory *failedProcessDirectory) ReadDir(int) ([]os.DirEntry, error) {
	return nil, directory.err
}

func (directory *failedProcessDirectory) Close() error { return directory.closeErr }

type failedProcessStat struct {
	source   []byte
	readErr  error
	closeErr error
}

func (stat *failedProcessStat) Read(destination []byte) (int, error) {
	if stat.readErr != nil {
		return 0, stat.readErr
	}
	read := copy(destination, stat.source)
	stat.source = stat.source[read:]
	if len(stat.source) == 0 {
		return read, io.EOF
	}
	return read, nil
}

func (stat *failedProcessStat) Close() error { return stat.closeErr }
