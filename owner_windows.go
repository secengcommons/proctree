//go:build windows

package proctree

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type processCreation struct {
	application    *uint16
	commandLine    *uint16
	inheritHandles bool
	flags          uint32
	environment    *uint16
	directory      *uint16
	startup        *windows.StartupInfo
}

type createProcessFunc func(processCreation, *windows.ProcessInformation) error

type processAttributeList interface {
	Update(uintptr, unsafe.Pointer, uintptr) error
	Delete()
	List() *windows.ProcThreadAttributeList
}

type nativeAPI struct {
	createJobObject     func(*windows.SecurityAttributes, *uint16) (windows.Handle, error)
	setJobInformation   func(windows.Handle, uint32, uintptr, uint32) (int, error)
	closeHandle         func(windows.Handle) error
	terminateJob        func(windows.Handle, uint32) error
	waitForSingleObject func(windows.Handle, uint32) (uint32, error)
	duplicateHandle     func(windows.Handle, windows.Handle, windows.Handle, *windows.Handle, uint32, bool, uint32) error
	newAttributeList    func(uint32) (processAttributeList, error)
	createProcess       createProcessFunc
	findProcess         func(int) (*os.Process, error)
	resumeThread        func(windows.Handle) (uint32, error)
	releaseProcess      func(*os.Process) error
	inputPipe           func() (*os.File, *os.File, error)
	now                 func() time.Time
}

func systemNativeAPI() nativeAPI {
	return nativeAPI{
		createJobObject: windows.CreateJobObject, setJobInformation: windows.SetInformationJobObject,
		closeHandle: windows.CloseHandle, terminateJob: windows.TerminateJobObject,
		waitForSingleObject: windows.WaitForSingleObject, duplicateHandle: windows.DuplicateHandle,
		newAttributeList: func(count uint32) (processAttributeList, error) { return windows.NewProcThreadAttributeList(count) },
		createProcess: func(creation processCreation, process *windows.ProcessInformation) error {
			return windows.CreateProcess(
				creation.application,
				creation.commandLine,
				nil,
				nil,
				creation.inheritHandles,
				creation.flags,
				creation.environment,
				creation.directory,
				creation.startup,
				process,
			)
		},
		findProcess: os.FindProcess, resumeThread: windows.ResumeThread,
		releaseProcess: func(process *os.Process) error { return process.Release() }, inputPipe: inputPipe, now: time.Now,
	}
}

type windowsOwner struct {
	job        windows.Handle
	inputDone  <-chan error
	inputWrite *os.File
	inputOnce  sync.Once
	inputErr   error
	startupErr error
	api        nativeAPI
}

func newProcessOwner() (processOwner, error) {
	return newWindowsOwner(systemNativeAPI())
}

func newWindowsOwner(api nativeAPI) (processOwner, error) {
	job, err := api.createJobObject(nil, nil)
	if err != nil {
		return nil, errors.Join(ErrOwnership, fmt.Errorf("create Job Object: %w", err))
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = api.setJobInformation(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), //nolint:gosec // Windows requires the native Job information address
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		return nil, errors.Join(ErrOwnership, fmt.Errorf("configure Job Object: %w", err), cleanupError(api.closeHandle(job)))
	}
	runtime.KeepAlive(&limits)
	return &windowsOwner{job: job, api: api}, nil
}

func (owner *windowsOwner) start(command *exec.Cmd, admitted Command) error {
	started, err := startAtomicWindowsProcess(command, admitted, owner.job, owner.api)
	owner.inputDone = started.inputDone
	owner.inputWrite = started.inputWrite
	owner.startupErr = started.startupErr
	return err
}

func (owner *windowsOwner) terminate() error {
	terminateErr := owner.api.terminateJob(owner.job, 1)
	if terminateErr != nil {
		terminateErr = fmt.Errorf("terminate Job Object: %w", terminateErr)
	}
	return errors.Join(terminateErr, owner.closeInput())
}

func (owner *windowsOwner) wait(deadline time.Time) error {
	milliseconds, err := durationMilliseconds(max(0, deadline.Sub(owner.api.now())))
	if err != nil {
		return err
	}
	state, waitErr := owner.api.waitForSingleObject(owner.job, milliseconds)
	if waitErr != nil {
		waitErr = fmt.Errorf("wait for Job Object: %w", waitErr)
	} else if state != uint32(windows.WAIT_OBJECT_0) {
		waitErr = errors.New("job object termination deadline exceeded")
	}
	return errors.Join(waitErr, waitForInput(owner.inputDone, deadline, owner.api.now))
}

func (owner *windowsOwner) close(time.Time) error {
	return errors.Join(owner.startupErr, owner.closeInput(), owner.api.closeHandle(owner.job))
}

func (owner *windowsOwner) closeInput() error {
	owner.inputOnce.Do(func() { owner.inputErr = closeFile(owner.inputWrite) })
	return owner.inputErr
}

func (*windowsOwner) health() <-chan struct{} { return nil }
func (*windowsOwner) healthError() error      { return nil }

func waitForInput(done <-chan error, deadline time.Time, now func() time.Time) error {
	if done == nil {
		return nil
	}
	select {
	case err := <-done:
		return err
	default:
	}
	timer := time.NewTimer(max(0, deadline.Sub(now())))
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return inputAfterDeadline(done)
	}
}

func inputAfterDeadline(done <-chan error) error {
	select {
	case err := <-done:
		return err
	default:
		return errors.New("process input did not close before cleanup deadline")
	}
}

func durationMilliseconds(duration time.Duration) (uint32, error) {
	if duration <= 0 {
		return 0, nil
	}
	milliseconds := duration / time.Millisecond
	if duration%time.Millisecond != 0 {
		milliseconds++
	}
	if milliseconds > math.MaxUint32 {
		return 0, ErrCleanup
	}
	return uint32(milliseconds), nil
}
