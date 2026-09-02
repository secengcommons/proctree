//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package proctree

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestDispatchWatchdog(t *testing.T) {
	if handled, code := dispatchWatchdog(nil, watchdogAPI{}); handled || code != 0 {
		t.Fatalf("ordinary dispatch = (%t, %d)", handled, code)
	}
	api := successfulWatchdogAPI(t)
	if handled, code := dispatchWatchdog([]string{"proctree", watchdogArgument}, api); !handled || code != 0 {
		t.Fatalf("successful dispatch = (%t, %d)", handled, code)
	}
	api = successfulWatchdogAPI(t)
	api.kill = func(int, syscall.Signal) error { return errors.New("kill") }
	if handled, code := dispatchWatchdog([]string{"proctree", watchdogArgument}, api); !handled || code != 1 {
		t.Fatalf("failed dispatch = (%t, %d)", handled, code)
	}
}

func TestRunWatchdogRejectsInvalidControl(t *testing.T) {
	tests := []struct {
		name string
		api  func(*testing.T) watchdogAPI
	}{
		{name: "missing", api: func(*testing.T) watchdogAPI {
			api := systemWatchdogAPI()
			api.file = func(uintptr, string) *os.File { return nil }
			return api
		}},
		{name: "closed", api: func(t *testing.T) watchdogAPI {
			file, err := os.CreateTemp(t.TempDir(), "closed-")
			if err != nil {
				t.Fatal(err)
			}
			if err = file.Close(); err != nil {
				t.Fatal(err)
			}
			api := systemWatchdogAPI()
			api.file = func(uintptr, string) *os.File { return file }
			return api
		}},
		{name: "regular", api: func(t *testing.T) watchdogAPI {
			file, err := os.CreateTemp(t.TempDir(), "regular-")
			if err != nil {
				t.Fatal(err)
			}
			api := systemWatchdogAPI()
			api.file = func(uintptr, string) *os.File { return file }
			return api
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := runWatchdog(test.api(t)); err == nil {
				t.Fatal("runWatchdog accepted invalid control")
			}
		})
	}
}

func TestRunWatchdogRejectsInvalidReadiness(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		controlRead, controlWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = controlWrite.Close(); err != nil {
			t.Fatal(err)
		}
		api := systemWatchdogAPI()
		api.file = func(descriptor uintptr, _ string) *os.File {
			if descriptor == watchdogControlDescriptor {
				return controlRead
			}
			return nil
		}
		if err = runWatchdog(api); err == nil {
			t.Fatal("missing readiness descriptor accepted")
		}
	})
	t.Run("regular", func(t *testing.T) {
		controlRead, controlWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = controlWrite.Close(); err != nil {
			t.Fatal(err)
		}
		regular, err := os.CreateTemp(t.TempDir(), "readiness-")
		if err != nil {
			t.Fatal(err)
		}
		api := systemWatchdogAPI()
		api.file = func(descriptor uintptr, _ string) *os.File {
			if descriptor == watchdogControlDescriptor {
				return controlRead
			}
			return regular
		}
		if err = runWatchdog(api); err == nil {
			t.Fatal("regular readiness descriptor accepted")
		}
	})
}

func TestRunWatchdogRejectsGroupAndDrainFailures(t *testing.T) {
	t.Run("group", func(t *testing.T) {
		api := watchdogPipeAPI(t)
		failure := errors.New("group")
		api.group = func() (int, error) { return 0, failure }
		if err := runWatchdog(api); !errors.Is(err, failure) {
			t.Fatalf("group error = %v", err)
		}
	})
	t.Run("leader", func(t *testing.T) {
		api := watchdogPipeAPI(t)
		api.pid = func() int { return 42 }
		api.group = func() (int, error) { return 41, nil }
		if err := runWatchdog(api); err == nil {
			t.Fatal("non-leader accepted")
		}
	})
	t.Run("identity", func(t *testing.T) {
		api := watchdogPipeAPI(t)
		api.pid = func() int { return 0 }
		api.group = func() (int, error) { return 0, nil }
		if err := runWatchdog(api); err == nil {
			t.Fatal("zero identity accepted")
		}
	})
	t.Run("drain", func(t *testing.T) {
		api := watchdogPipeAPI(t)
		failure := errors.New("drain")
		api.drain = func(control *os.File) error { return errors.Join(failure, control.Close()) }
		if err := runWatchdog(api); !errors.Is(err, failure) {
			t.Fatalf("drain error = %v", err)
		}
	})
	t.Run("ready", func(t *testing.T) {
		api := watchdogPipeAPI(t)
		failure := errors.New("ready")
		api.ready = func(file *os.File) error { return errors.Join(failure, file.Close()) }
		if err := runWatchdog(api); !errors.Is(err, failure) {
			t.Fatalf("readiness error = %v", err)
		}
	})
}

func TestDrainWatchdog(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = write.Close(); err != nil {
		t.Fatal(err)
	}
	if err = drainWatchdog(read); err != nil {
		t.Fatal(err)
	}
	closed, err := os.CreateTemp(t.TempDir(), "closed-")
	if err != nil {
		t.Fatal(err)
	}
	if err = closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err = drainWatchdog(closed); err == nil {
		t.Fatal("closed descriptor drained")
	}
}

func TestSystemWatchdogAPI(t *testing.T) {
	api := systemWatchdogAPI()
	if api.file == nil || api.pid() <= 0 || api.kill == nil || api.drain == nil || api.ready == nil {
		t.Fatal("system watchdog API is incomplete")
	}
	if _, err := api.group(); err != nil {
		t.Fatal(err)
	}
}

func TestSignalWatchdogReady(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = signalWatchdogReady(write); err != nil {
		t.Fatal(err)
	}
	var signal [1]byte
	if _, err = read.Read(signal[:]); err != nil || signal[0] != watchdogReadySignal {
		t.Fatalf("readiness signal = (%q, %v)", signal, err)
	}
	if err = read.Close(); err != nil {
		t.Fatal(err)
	}
}

func successfulWatchdogAPI(t *testing.T) watchdogAPI {
	t.Helper()
	api := watchdogPipeAPI(t)
	api.drain = func(control *os.File) error { return control.Close() }
	api.kill = func(processID int, signal syscall.Signal) error {
		if processID != -41 || signal != syscall.SIGKILL {
			t.Fatalf("kill = (%d, %d)", processID, signal)
		}
		return nil
	}
	return api
}

func watchdogPipeAPI(t *testing.T) watchdogAPI {
	t.Helper()
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = controlWrite.Close(); err != nil {
		t.Fatal(err)
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := readyRead.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			t.Errorf("close readiness fixture: %v", closeErr)
		}
	})
	api := systemWatchdogAPI()
	api.file = func(descriptor uintptr, name string) *os.File {
		switch descriptor {
		case watchdogControlDescriptor:
			if name != "proctree-controller" {
				t.Fatalf("control name = %q", name)
			}
			return controlRead
		case watchdogReadyDescriptor:
			if name != "proctree-ready" {
				t.Fatalf("readiness name = %q", name)
			}
			return readyWrite
		default:
			t.Fatalf("descriptor = %d", descriptor)
			return nil
		}
	}
	api.ready = func(file *os.File) error { return file.Close() }
	api.pid = func() int { return 41 }
	api.group = func() (int, error) { return 41, nil }
	api.kill = func(int, syscall.Signal) error { return nil }
	return api
}
