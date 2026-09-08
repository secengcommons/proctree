//go:build windows

package proctree

import (
	"errors"
	"math"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const fixtureThreadSuspendCount = 1

func TestWindowsOwnerCreationFailures(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		api := systemNativeAPI()
		failure := errors.New("create")
		api.createJobObject = func(*windows.SecurityAttributes, *uint16) (windows.Handle, error) { return 0, failure }
		if _, err := newWindowsOwner(api); !errors.Is(err, ErrOwnership) || !errors.Is(err, failure) {
			t.Fatalf("newWindowsOwner error = %v", err)
		}
	})
	t.Run("configure", func(t *testing.T) {
		api := systemNativeAPI()
		configureErr := errors.New("configure")
		closeErr := errors.New("close")
		api.createJobObject = func(*windows.SecurityAttributes, *uint16) (windows.Handle, error) { return 1, nil }
		api.setJobInformation = func(windows.Handle, uint32, uintptr, uint32) (int, error) { return 0, configureErr }
		api.closeHandle = func(windows.Handle) error { return closeErr }
		if _, err := newWindowsOwner(api); !errors.Is(err, configureErr) || !errors.Is(err, closeErr) || !errors.Is(err, ErrCleanup) {
			t.Fatalf("newWindowsOwner error = %v", err)
		}
	})
}

func TestSystemNativeAPIReleasesProcess(t *testing.T) {
	api := systemNativeAPI()
	process, err := api.findProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err = api.releaseProcess(process); err != nil {
		t.Fatal(err)
	}
	owner := &windowsOwner{}
	if owner.health() != nil || owner.healthError() != nil {
		t.Fatal("Windows owner exposed watchdog health")
	}
}

func TestAtomicWindowsProcessUsesJobAndHandleLists(t *testing.T) {
	command, admitted, api, attributes := atomicWindowsFixture(t)
	created := false
	api.createProcess = assertedAtomicCreateProcess(t, attributes, &created)
	started, err := startAtomicWindowsProcess(command, admitted, 1, api)
	if err != nil || started.startupErr != nil || !created || command.Process == nil || !attributes.deleted {
		t.Fatalf("startAtomicWindowsProcess = (%v, %v, %t, %#v, %t)", err, started.startupErr, created, command.Process, attributes.deleted)
	}
	if err = waitForInput(started.inputDone, time.Now().Add(DefaultCleanupTimeout), time.Now); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicWindowsProcessRefusesIncompatibleJobAdmission(t *testing.T) {
	command, admitted, api, attributes := atomicWindowsFixture(t)
	failure := errors.New("job admission")
	attributes.failAt = 2
	attributes.failure = failure
	created := false
	api.createProcess = func(processCreation, *windows.ProcessInformation) error {
		created = true
		return nil
	}
	_, err := startAtomicWindowsProcess(command, admitted, 1, api)
	if !errors.Is(err, ErrOwnership) || !errors.Is(err, failure) || created {
		t.Fatalf("incompatible Job admission = (%t, %v)", created, err)
	}
}

func TestAtomicWindowsProcessFailures(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*exec.Cmd, *nativeAPI, *fixtureAttributeList, error)
		want   error
	}{
		{name: "output", modify: func(command *exec.Cmd, _ *nativeAPI, _ *fixtureAttributeList, _ error) { command.Stdout = nil }, want: ErrOwnership},
		{name: "pipe", modify: func(_ *exec.Cmd, api *nativeAPI, _ *fixtureAttributeList, failure error) {
			api.inputPipe = func() (*os.File, *os.File, error) { return nil, nil, failure }
		}, want: ErrOwnership},
		{name: "duplicate", modify: func(_ *exec.Cmd, api *nativeAPI, _ *fixtureAttributeList, failure error) {
			api.duplicateHandle = func(windows.Handle, windows.Handle, windows.Handle, *windows.Handle, uint32, bool, uint32) error {
				return failure
			}
		}, want: ErrOwnership},
		{name: "attributes", modify: func(_ *exec.Cmd, api *nativeAPI, _ *fixtureAttributeList, failure error) {
			api.newAttributeList = func(uint32) (processAttributeList, error) { return nil, failure }
		}, want: ErrOwnership},
		{name: "handle list", modify: func(_ *exec.Cmd, _ *nativeAPI, attributes *fixtureAttributeList, _ error) { attributes.failAt = 1 }, want: ErrOwnership},
		{name: "job list", modify: func(_ *exec.Cmd, _ *nativeAPI, attributes *fixtureAttributeList, _ error) { attributes.failAt = 2 }, want: ErrOwnership},
		{name: "create", modify: func(_ *exec.Cmd, api *nativeAPI, _ *fixtureAttributeList, failure error) {
			api.createProcess = failingCreateProcess(failure)
		}, want: ErrStart},
		{name: "find", modify: func(_ *exec.Cmd, api *nativeAPI, _ *fixtureAttributeList, failure error) {
			api.findProcess = func(int) (*os.Process, error) { return nil, failure }
		}, want: ErrOwnership},
		{name: "resume", modify: func(_ *exec.Cmd, api *nativeAPI, _ *fixtureAttributeList, failure error) {
			api.resumeThread = func(windows.Handle) (uint32, error) { return 0, failure }
		}, want: ErrStart},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, admitted, api, attributes := atomicWindowsFixture(t)
			failure := errors.New(test.name)
			attributes.failure = failure
			test.modify(command, &api, attributes, failure)
			_, err := startAtomicWindowsProcess(command, admitted, 1, api)
			if !errors.Is(err, test.want) || (test.name != "output" && !errors.Is(err, failure)) {
				t.Fatalf("start failure = %v", err)
			}
			if test.name == "find" || test.name == "resume" {
				if bounded, ok := errors.AsType[*boundedStartError](err); !ok || bounded == nil {
					t.Fatalf("start failure has no cleanup deadline: %v", err)
				}
			}
		})
	}
}

func TestAtomicWindowsProcessClosesOwnedHandlesOnce(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*nativeAPI, *windowsHandleCloseTracker, error)
	}{
		{name: "partial parent cleanup", modify: func(_ *nativeAPI, tracker *windowsHandleCloseTracker, failure error) {
			tracker.firstFailure = failure
		}},
		{name: "find", modify: func(api *nativeAPI, _ *windowsHandleCloseTracker, failure error) {
			api.findProcess = func(int) (*os.Process, error) { return nil, failure }
		}},
		{name: "resume", modify: func(api *nativeAPI, _ *windowsHandleCloseTracker, failure error) {
			api.resumeThread = func(windows.Handle) (uint32, error) { return 0, failure }
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { testAtomicWindowsHandleOwnership(t, test.modify) })
	}
}

func testAtomicWindowsHandleOwnership(
	t *testing.T,
	modify func(*nativeAPI, *windowsHandleCloseTracker, error),
) {
	t.Helper()
	command, admitted, api, attributes := atomicWindowsFixture(t)
	failure := errors.New("injected failure")
	tracker := &windowsHandleCloseTracker{
		attributes: attributes, closed: make(map[windows.Handle]bool), doubleClose: errors.New("handle closed twice"),
	}
	api.closeHandle = tracker.close
	modify(&api, tracker, failure)
	_, err := startAtomicWindowsProcess(command, admitted, 1, api)
	if !errors.Is(err, failure) || errors.Is(err, tracker.doubleClose) || errors.Is(err, os.ErrClosed) || errors.Is(err, ErrCleanup) {
		t.Fatalf("handle ownership error = %v", err)
	}
	tracker.assertClosed(t)
}

type windowsHandleCloseTracker struct {
	attributes   *fixtureAttributeList
	closed       map[windows.Handle]bool
	doubleClose  error
	firstFailure error
	failedFirst  bool
}

func (tracker *windowsHandleCloseTracker) close(handle windows.Handle) error {
	if tracker.closed[handle] {
		return tracker.doubleClose
	}
	if tracker.firstFailure != nil && !tracker.failedFirst && len(tracker.attributes.handles) != 0 && handle == tracker.attributes.handles[0] {
		tracker.failedFirst = true
		return tracker.firstFailure
	}
	tracker.closed[handle] = true
	return nil
}

func (tracker *windowsHandleCloseTracker) assertClosed(t *testing.T) {
	t.Helper()
	for _, handle := range append(append([]windows.Handle(nil), tracker.attributes.handles...), 2, 3) {
		if !tracker.closed[handle] {
			t.Fatalf("handle %d was not closed", handle)
		}
	}
}

func TestAtomicWindowsProcessRetainsStartupCleanup(t *testing.T) {
	command, admitted, api, _ := atomicWindowsFixture(t)
	closeErr := errors.New("close")
	api.closeHandle = func(handle windows.Handle) error {
		if handle == 2 || handle == 3 {
			return closeErr
		}
		return nil
	}
	started, err := startAtomicWindowsProcess(command, admitted, 1, api)
	if err != nil || !errors.Is(started.startupErr, closeErr) {
		t.Fatalf("start cleanup = (%v, %v)", err, started.startupErr)
	}
	if err = waitForInput(started.inputDone, time.Now().Add(DefaultCleanupTimeout), time.Now); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicWindowsProcessRejectsParentHandleCleanup(t *testing.T) {
	command, admitted, api, _ := atomicWindowsFixture(t)
	failure := errors.New("clear")
	resumed := false
	api.closeHandle = func(windows.Handle) error { return failure }
	api.resumeThread = func(windows.Handle) (uint32, error) { resumed = true; return fixtureThreadSuspendCount, nil }
	_, err := startAtomicWindowsProcess(command, admitted, 1, api)
	var bounded *boundedStartError
	if resumed || !errors.Is(err, ErrOwnership) || !errors.Is(err, failure) || !errors.As(err, &bounded) {
		t.Fatalf("parent handle cleanup = (%t, %v)", resumed, err)
	}
}

func TestAtomicWindowsProcessAcceptsAlreadyResumedThread(t *testing.T) {
	command, admitted, api, _ := atomicWindowsFixture(t)
	api.resumeThread = func(windows.Handle) (uint32, error) { return 0, nil }
	started, err := startAtomicWindowsProcess(command, admitted, 1, api)
	if err != nil {
		t.Fatal(err)
	}
	if err = waitForInput(started.inputDone, time.Now().Add(DefaultCleanupTimeout), time.Now); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicWindowsProcessPreservesExternalSuspension(t *testing.T) {
	command, admitted, api, _ := atomicWindowsFixture(t)
	api.resumeThread = func(windows.Handle) (uint32, error) { return fixtureThreadSuspendCount + 1, nil }
	started, err := startAtomicWindowsProcess(command, admitted, 1, api)
	if err != nil {
		t.Fatal(err)
	}
	if err = waitForInput(started.inputDone, time.Now().Add(DefaultCleanupTimeout), time.Now); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicWindowsProcessRetainsFindCleanupFailure(t *testing.T) {
	command, admitted, api, _ := atomicWindowsFixture(t)
	findErr := errors.New("find")
	waitErr := errors.New("wait")
	resumed := false
	api.findProcess = func(int) (*os.Process, error) { return nil, findErr }
	api.resumeThread = func(windows.Handle) (uint32, error) { resumed = true; return fixtureThreadSuspendCount, nil }
	api.waitForSingleObject = func(windows.Handle, uint32) (uint32, error) { return 0, waitErr }
	_, err := startAtomicWindowsProcess(command, admitted, 1, api)
	if resumed || !errors.Is(err, ErrOwnership) || !errors.Is(err, ErrCleanup) || !errors.Is(err, findErr) || !errors.Is(err, waitErr) {
		t.Fatalf("find cleanup error = %v", err)
	}
}

func TestWaitForWindowsProcess(t *testing.T) {
	api := systemNativeAPI()
	api.waitForSingleObject = func(windows.Handle, uint32) (uint32, error) { return uint32(windows.WAIT_OBJECT_0), nil }
	if err := waitForWindowsProcess(1, api.now(), api); err != nil {
		t.Fatal(err)
	}
	api.waitForSingleObject = func(windows.Handle, uint32) (uint32, error) { return uint32(windows.WAIT_TIMEOUT), nil }
	if err := waitForWindowsProcess(1, api.now(), api); err == nil {
		t.Fatal("process timeout accepted")
	}
	if err := waitForWindowsProcess(1, api.now().Add(time.Duration(math.MaxInt64)), api); !errors.Is(err, ErrCleanup) {
		t.Fatalf("process duration error = %v", err)
	}
}

func TestWindowsEnvironmentBlock(t *testing.T) {
	empty, err := windowsEnvironmentBlock(nil)
	if err != nil || !reflect.DeepEqual(empty, []uint16{0, 0}) {
		t.Fatalf("empty environment = (%#v, %v)", empty, err)
	}
	block, err := windowsEnvironmentBlock([]string{"value=2", "NAME=1"})
	if err != nil {
		t.Fatal(err)
	}
	wantName, err := windows.UTF16FromString("NAME=1")
	if err != nil {
		t.Fatal(err)
	}
	wantValue, err := windows.UTF16FromString("value=2")
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(wantName, wantValue...), 0)
	if !reflect.DeepEqual(block, want) {
		t.Fatalf("environment block = %#v", block)
	}
	if _, err = windowsEnvironmentBlock([]string{"INVALID\x00=value"}); err == nil {
		t.Fatal("environment NUL accepted")
	}
}

func TestCreateAtomicWindowsProcessRejectsInvalidMaterial(t *testing.T) {
	_, _, api, attributes := atomicWindowsFixture(t)
	tests := []Command{
		{Executable: "invalid\x00executable"},
		{Executable: `C:\fixture.exe`, Arguments: []string{"invalid\x00argument"}},
		{Executable: `C:\fixture.exe`, Environment: []string{"INVALID\x00=value"}},
		{Executable: `C:\fixture.exe`, Directory: "invalid\x00directory"},
	}
	for index, admitted := range tests {
		if _, err := createAtomicWindowsProcess(admitted, []windows.Handle{1, 2, 3}, attributes, api); err == nil {
			t.Fatalf("invalid material %d accepted", index)
		}
	}
}

func TestWriteProcessInput(t *testing.T) {
	read, write, err := inputPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = writeProcessInput(write, []byte("input")); err != nil {
		t.Fatal(err)
	}
	var value [5]byte
	if _, err = read.Read(value[:]); err != nil || string(value[:]) != "input" {
		t.Fatalf("input = (%q, %v)", value, err)
	}
	if err = closeFile(read); err != nil {
		t.Fatal(err)
	}
	read, write, err = inputPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = read.Close(); err != nil {
		t.Fatal(err)
	}
	if err = writeProcessInput(write, []byte("input")); err != nil {
		t.Fatalf("broken pipe error = %v", err)
	}
}

func TestWindowsOwnerTerminationCancelsInput(t *testing.T) {
	read, write, err := inputPipe()
	if err != nil {
		t.Fatal(err)
	}
	inputDone := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		inputDone <- writeProcessInput(write, make([]byte, MaxInputBytes))
	}()
	<-started
	api := systemNativeAPI()
	api.terminateJob = func(windows.Handle, uint32) error { return nil }
	owner := &windowsOwner{job: 1, inputDone: inputDone, inputWrite: write, api: api}
	if err = owner.terminate(); err != nil {
		t.Fatal(err)
	}
	if err = waitForInput(inputDone, time.Now().Add(outputSafetyTimeout), time.Now); err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(owner.closeInput(), closeFile(read)); err != nil {
		t.Fatal(err)
	}
}

func TestCloseWindowsHandlesAttemptsEveryHandle(t *testing.T) {
	failure := errors.New("clear")
	api := systemNativeAPI()
	var attempted []windows.Handle
	api.closeHandle = func(handle windows.Handle) error {
		attempted = append(attempted, handle)
		if handle == 1 {
			return failure
		}
		return nil
	}
	handles := []windows.Handle{1, 2, 3}
	err := closeWindowsHandles(handles, api)
	if !errors.Is(err, failure) || !reflect.DeepEqual(attempted, []windows.Handle{1, 2, 3}) || !reflect.DeepEqual(handles, []windows.Handle{1, 0, 0}) {
		t.Fatalf("inheritance cleanup = (%v, %v, %v)", handles, attempted, err)
	}
}

func TestDuplicateWindowsHandlesCleansPartialResult(t *testing.T) {
	failure := errors.New("duplicate")
	closeFailure := errors.New("close")
	api := systemNativeAPI()
	var closed []windows.Handle
	api.duplicateHandle = func(_ windows.Handle, source windows.Handle, _ windows.Handle, target *windows.Handle, _ uint32, _ bool, _ uint32) error {
		if source == 2 {
			return failure
		}
		*target = source + 10
		return nil
	}
	api.closeHandle = func(handle windows.Handle) error { closed = append(closed, handle); return closeFailure }
	if _, err := duplicateWindowsHandles([]windows.Handle{1, 2, 3}, api); !errors.Is(err, failure) || !errors.Is(err, ErrCleanup) ||
		!errors.Is(err, closeFailure) || !reflect.DeepEqual(closed, []windows.Handle{11}) {
		t.Fatalf("duplicate cleanup = (%v, %v)", closed, err)
	}
}

func TestWindowsOwnerTerminationFailures(t *testing.T) {
	t.Run("terminate", func(t *testing.T) {
		api := systemNativeAPI()
		failure := errors.New("terminate")
		api.terminateJob = func(windows.Handle, uint32) error { return failure }
		if err := (&windowsOwner{job: 1, api: api}).terminate(); !errors.Is(err, failure) {
			t.Fatalf("terminate error = %v", err)
		}
	})
	t.Run("wait error", func(t *testing.T) {
		api := systemNativeAPI()
		failure := errors.New("wait")
		inputFailure := errors.New("input")
		api.waitForSingleObject = func(windows.Handle, uint32) (uint32, error) { return 0, failure }
		done := make(chan error, 1)
		done <- inputFailure
		if err := (&windowsOwner{job: 1, inputDone: done, api: api}).wait(api.now().Add(time.Second)); !errors.Is(err, failure) || !errors.Is(err, inputFailure) {
			t.Fatalf("wait error = %v", err)
		}
	})
	t.Run("wait deadline", func(t *testing.T) {
		api := systemNativeAPI()
		api.waitForSingleObject = func(windows.Handle, uint32) (uint32, error) { return uint32(windows.WAIT_TIMEOUT), nil }
		if err := (&windowsOwner{job: 1, api: api}).wait(api.now().Add(time.Second)); err == nil {
			t.Fatal("wait deadline accepted")
		}
	})
	t.Run("input", func(t *testing.T) {
		api := systemNativeAPI()
		api.waitForSingleObject = func(windows.Handle, uint32) (uint32, error) { return uint32(windows.WAIT_OBJECT_0), nil }
		done := make(chan error, 1)
		failure := errors.New("input")
		done <- failure
		if err := (&windowsOwner{job: 1, inputDone: done, api: api}).wait(api.now().Add(time.Second)); !errors.Is(err, failure) {
			t.Fatalf("input error = %v", err)
		}
	})
}

func TestWaitForInput(t *testing.T) {
	if err := waitForInput(nil, time.Time{}, time.Now); err != nil {
		t.Fatal(err)
	}
	if err := waitForInput(make(chan error), time.Now(), time.Now); err == nil {
		t.Fatal("input deadline accepted")
	}
	done := make(chan error, 1)
	done <- nil
	if err := inputAfterDeadline(done); err != nil {
		t.Fatal(err)
	}
}

func TestDurationMilliseconds(t *testing.T) {
	t.Parallel()
	for duration, want := range map[time.Duration]uint32{-1: 0, 0: 0, time.Nanosecond: 1, time.Millisecond + 1: 2} {
		value, err := durationMilliseconds(duration)
		if err != nil || value != want {
			t.Fatalf("duration %s = (%d, %v)", duration, value, err)
		}
	}
	if _, err := durationMilliseconds(time.Duration(math.MaxInt64)); !errors.Is(err, ErrCleanup) {
		t.Fatalf("excess duration error = %v", err)
	}
	api := systemNativeAPI()
	if err := (&windowsOwner{job: 1, api: api}).wait(api.now().Add(time.Duration(math.MaxInt64))); !errors.Is(err, ErrCleanup) {
		t.Fatalf("excess wait error = %v", err)
	}
}

type fixtureAttributeList struct {
	list    windows.ProcThreadAttributeList
	updates []uintptr
	handles []windows.Handle
	job     windows.Handle
	failAt  int
	failure error
	deleted bool
}

func (attributes *fixtureAttributeList) Update(attribute uintptr, value unsafe.Pointer, size uintptr) error {
	attributes.updates = append(attributes.updates, attribute)
	if attributes.failAt == len(attributes.updates) {
		return attributes.failure
	}
	switch attribute {
	case windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST:
		count := size / unsafe.Sizeof(windows.Handle(0))
		attributes.handles = append(
			attributes.handles,
			unsafe.Slice((*windows.Handle)(value), count)..., //nolint:gosec // The fixture inspects the native handle-list value
		)
	case procThreadAttributeJobList:
		attributes.job = *(*windows.Handle)(value)
	}
	return nil
}

func (attributes *fixtureAttributeList) Delete() { attributes.deleted = true }

func (attributes *fixtureAttributeList) List() *windows.ProcThreadAttributeList {
	return &attributes.list
}

func atomicWindowsFixture(t *testing.T) (*exec.Cmd, Command, nativeAPI, *fixtureAttributeList) {
	t.Helper()
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := errors.Join(closeFile(stdoutRead), closeFile(stdoutWrite), closeFile(stderrRead), closeFile(stderrWrite)); closeErr != nil {
			t.Errorf("close output fixture: %v", closeErr)
		}
	})
	attributes := &fixtureAttributeList{}
	api := systemNativeAPI()
	api.duplicateHandle = func(_ windows.Handle, source windows.Handle, _ windows.Handle, target *windows.Handle, _ uint32, inherit bool, options uint32) error {
		if !inherit || options != windows.DUPLICATE_SAME_ACCESS {
			t.Fatalf("duplicate options = (%t, %#x)", inherit, options)
		}
		*target = source + 100
		return nil
	}
	api.newAttributeList = func(uint32) (processAttributeList, error) { return attributes, nil }
	api.createProcess = func(_ processCreation, process *windows.ProcessInformation) error {
		process.Process = 2
		process.Thread = 3
		process.ProcessId = 4
		return nil
	}
	api.findProcess = func(int) (*os.Process, error) { return &os.Process{Pid: 4}, nil }
	api.resumeThread = func(windows.Handle) (uint32, error) { return fixtureThreadSuspendCount, nil }
	api.releaseProcess = func(*os.Process) error { return nil }
	api.closeHandle = func(windows.Handle) error { return nil }
	api.terminateJob = func(windows.Handle, uint32) error { return nil }
	api.waitForSingleObject = func(windows.Handle, uint32) (uint32, error) { return uint32(windows.WAIT_OBJECT_0), nil }
	command := &exec.Cmd{Stdout: stdoutWrite, Stderr: stderrWrite}
	admitted := Command{
		Executable: `C:\fixture.exe`, Arguments: []string{"argument"}, Environment: []string{"VALUE=one"},
		Input: []byte("input"), CleanupTimeout: DefaultCleanupTimeout,
	}
	return command, admitted, api, attributes
}

func assertedAtomicCreateProcess(t *testing.T, attributes *fixtureAttributeList, created *bool) createProcessFunc {
	t.Helper()
	return func(
		creation processCreation,
		process *windows.ProcessInformation,
	) error {
		*created = true
		if !creation.inheritHandles || creation.flags&windows.EXTENDED_STARTUPINFO_PRESENT == 0 || creation.flags&windows.CREATE_SUSPENDED == 0 ||
			creation.startup.Flags&windows.STARTF_USESTDHANDLES == 0 {
			t.Fatalf("creation flags = (%t, %#x, %#v)", creation.inheritHandles, creation.flags, *creation.startup)
		}
		want := []uintptr{windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, procThreadAttributeJobList}
		if !reflect.DeepEqual(attributes.updates, want) || len(attributes.handles) != 3 || attributes.job != 1 {
			t.Fatalf("creation attributes = (%#v, %#v, %d)", attributes.updates, attributes.handles, attributes.job)
		}
		process.Process = 2
		process.Thread = 3
		process.ProcessId = 4
		return nil
	}
}

func failingCreateProcess(failure error) createProcessFunc {
	return func(processCreation, *windows.ProcessInformation) error {
		return failure
	}
}
