//go:build windows

package proctree

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The retained system-call dependency does not expose the Windows 10 Job-list attribute
const procThreadAttributeJobList = 0x0002000d
const processAttributeCount = 2

type atomicWindowsStart struct {
	inputDone  <-chan error
	inputWrite *os.File
	startupErr error
}

func startAtomicWindowsProcess(
	command *exec.Cmd,
	admitted Command,
	job windows.Handle,
	api nativeAPI,
) (atomicWindowsStart, error) {
	inputRead, inputWrite, childHandles, err := createWindowsChildHandles(command, api)
	if err != nil {
		return atomicWindowsStart{}, err
	}
	attributes, err := createWindowsProcessAttributes(childHandles, job, api)
	if err != nil {
		cleanupErr := closeWindowsChildResources(inputRead, inputWrite, childHandles, api)
		return atomicWindowsStart{}, atomicCreationError(ErrOwnership, err, cleanupErr)
	}
	defer func() {
		attributes.Delete()
		runtime.KeepAlive(childHandles)
		runtime.KeepAlive(&job)
	}()
	return startPreparedWindowsProcess(command, admitted, inputRead, inputWrite, childHandles, job, attributes, api)
}

func createWindowsChildHandles(command *exec.Cmd, api nativeAPI) (*os.File, *os.File, []windows.Handle, error) {
	stdout, stdoutOK := command.Stdout.(*os.File)
	stderr, stderrOK := command.Stderr.(*os.File)
	if !stdoutOK || !stderrOK {
		return nil, nil, nil, errors.Join(ErrOwnership, errors.New("process output handles are unavailable"))
	}
	inputRead, inputWrite, err := api.inputPipe()
	if err != nil {
		return nil, nil, nil, errors.Join(ErrOwnership, fmt.Errorf("create process input pipe: %w", err))
	}
	sources := []windows.Handle{windows.Handle(inputRead.Fd()), windows.Handle(stdout.Fd()), windows.Handle(stderr.Fd())}
	childHandles, err := duplicateWindowsHandles(sources, api)
	if err != nil {
		cleanupErr := errors.Join(closeFile(inputRead), closeFile(inputWrite))
		return nil, nil, nil, atomicCreationError(ErrOwnership, err, cleanupErr)
	}
	return inputRead, inputWrite, childHandles, nil
}

func createWindowsProcessAttributes(handles []windows.Handle, job windows.Handle, api nativeAPI) (processAttributeList, error) {
	attributes, err := api.newAttributeList(processAttributeCount)
	if err != nil {
		return nil, err
	}
	if err = attributes.Update(
		windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&handles[0]), //nolint:gosec // Windows requires an address-stable inherited-handle list
		uintptr(len(handles))*unsafe.Sizeof(handles[0]),
	); err != nil {
		attributes.Delete()
		return nil, err
	}
	if err = attributes.Update(
		procThreadAttributeJobList,
		unsafe.Pointer(&job), //nolint:gosec // Windows requires an address-stable Job handle
		unsafe.Sizeof(job),
	); err != nil {
		attributes.Delete()
		return nil, err
	}
	return attributes, nil
}

func startPreparedWindowsProcess(
	command *exec.Cmd,
	admitted Command,
	inputRead, inputWrite *os.File,
	childHandles []windows.Handle,
	job windows.Handle,
	attributes processAttributeList,
	api nativeAPI,
) (atomicWindowsStart, error) {
	process, err := createAtomicWindowsProcess(admitted, childHandles, attributes, api)
	if err != nil {
		cleanupErr := closeWindowsChildResources(inputRead, inputWrite, childHandles, api)
		return atomicWindowsStart{}, atomicCreationError(ErrStart, err, cleanupErr)
	}
	fail := func(class error, operation string, cause error, found *os.Process) (atomicWindowsStart, error) {
		deadline := api.now().Add(admitted.CleanupTimeout)
		cleanupErr := cleanupSuspendedWindowsProcess(process, found, inputRead, inputWrite, childHandles, job, deadline, api)
		resultErr := errors.Join(class, fmt.Errorf("%s: %w", operation, cause), cleanupError(cleanupErr))
		return atomicWindowsStart{}, withCleanupDeadline(resultErr, deadline)
	}
	preparationErr := closeWindowsHandles(childHandles, api)
	preparationErr = errors.Join(preparationErr, closeFile(inputRead))
	inputRead = nil
	if preparationErr != nil {
		deadline := api.now().Add(admitted.CleanupTimeout)
		cleanupErr := cleanupSuspendedWindowsProcess(process, nil, inputRead, inputWrite, childHandles, job, deadline, api)
		return atomicWindowsStart{}, withCleanupDeadline(atomicCreationError(ErrOwnership, preparationErr, cleanupErr), deadline)
	}
	found, findErr := api.findProcess(int(process.ProcessId))
	if findErr != nil {
		return fail(ErrOwnership, "open created process", findErr, nil)
	}
	// ResumeThread releases the count requested during creation without draining counts held by another owner
	if _, resumeErr := api.resumeThread(process.Thread); resumeErr != nil {
		return fail(ErrStart, "resume created process", resumeErr, found)
	}
	cleanupErr := errors.Join(
		api.closeHandle(process.Thread),
		api.closeHandle(process.Process),
	)
	command.Process = found
	inputDone := make(chan error, 1)
	if len(admitted.Input) == 0 {
		inputDone <- closeFile(inputWrite)
		inputWrite = nil
	} else {
		go func() { inputDone <- writeProcessInput(inputWrite, admitted.Input) }()
	}
	return atomicWindowsStart{inputDone: inputDone, inputWrite: inputWrite, startupErr: cleanupErr}, nil
}

func cleanupSuspendedWindowsProcess(
	process windows.ProcessInformation,
	found *os.Process,
	inputRead, inputWrite *os.File,
	childHandles []windows.Handle,
	job windows.Handle,
	deadline time.Time,
	api nativeAPI,
) error {
	terminateErr := api.terminateJob(job, 1)
	waitErr := waitForWindowsProcess(process.Process, deadline, api)
	var releaseErr error
	if found != nil {
		releaseErr = api.releaseProcess(found)
	}
	return errors.Join(
		terminateErr,
		waitErr,
		releaseErr,
		api.closeHandle(process.Thread),
		api.closeHandle(process.Process),
		closeWindowsHandles(childHandles, api),
		closeFile(inputRead),
		closeFile(inputWrite),
	)
}

func atomicCreationError(class, cause, cleanupErr error) error {
	return errors.Join(class, cause, cleanupError(cleanupErr))
}

func closeWindowsChildResources(inputRead, inputWrite *os.File, childHandles []windows.Handle, api nativeAPI) error {
	return errors.Join(closeWindowsHandles(childHandles, api), closeFile(inputRead), closeFile(inputWrite))
}

func waitForWindowsProcess(process windows.Handle, deadline time.Time, api nativeAPI) error {
	milliseconds, err := durationMilliseconds(max(0, deadline.Sub(api.now())))
	if err != nil {
		return err
	}
	state, err := api.waitForSingleObject(process, milliseconds)
	if err != nil {
		return err
	}
	if state != uint32(windows.WAIT_OBJECT_0) {
		return errors.New("created process did not terminate before cleanup deadline")
	}
	return nil
}

func createAtomicWindowsProcess(
	admitted Command,
	handles []windows.Handle,
	attributes processAttributeList,
	api nativeAPI,
) (windows.ProcessInformation, error) {
	application, err := windows.UTF16PtrFromString(admitted.Executable)
	if err != nil {
		return windows.ProcessInformation{}, err
	}
	arguments := append([]string{admitted.Executable}, admitted.Arguments...)
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(arguments))
	if err != nil {
		return windows.ProcessInformation{}, err
	}
	environment, err := windowsEnvironmentBlock(admitted.Environment)
	if err != nil {
		return windows.ProcessInformation{}, err
	}
	var directory *uint16
	if admitted.Directory != "" {
		directory, err = windows.UTF16PtrFromString(admitted.Directory)
		if err != nil {
			return windows.ProcessInformation{}, err
		}
	}
	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Flags: windows.STARTF_USESTDHANDLES,
			StdInput: handles[0], StdOutput: handles[1], StdErr: handles[2],
		},
		ProcThreadAttributeList: attributes.List(),
	}
	var process windows.ProcessInformation
	err = api.createProcess(processCreation{
		application:    application,
		commandLine:    commandLine,
		inheritHandles: true,
		flags: windows.CREATE_DEFAULT_ERROR_MODE | windows.CREATE_SUSPENDED |
			windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT,
		environment: &environment[0],
		directory:   directory,
		startup:     &startup.StartupInfo,
	}, &process)
	return process, err
}

func closeWindowsHandles(handles []windows.Handle, api nativeAPI) error {
	var result error
	for index, handle := range handles {
		if handle != 0 {
			if err := api.closeHandle(handle); err != nil {
				result = errors.Join(result, err)
			} else {
				handles[index] = 0
			}
		}
	}
	return result
}

func duplicateWindowsHandles(sources []windows.Handle, api nativeAPI) ([]windows.Handle, error) {
	process := windows.CurrentProcess()
	duplicates := make([]windows.Handle, len(sources))
	for index, source := range sources {
		if err := api.duplicateHandle(process, source, process, &duplicates[index], 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return nil, errors.Join(err, cleanupError(closeWindowsHandles(duplicates[:index], api)))
		}
	}
	return duplicates, nil
}

func windowsEnvironmentBlock(environment []string) ([]uint16, error) {
	sorted := append([]string(nil), environment...)
	slices.SortFunc(sorted, func(left, right string) int {
		leftName, _, _ := strings.Cut(left, "=")
		rightName, _, _ := strings.Cut(right, "=")
		return cmp.Compare(strings.ToUpper(leftName), strings.ToUpper(rightName))
	})
	if len(sorted) == 0 {
		return []uint16{0, 0}, nil
	}
	capacity := 1
	for _, entry := range sorted {
		capacity += len(entry) + 1
	}
	block := make([]uint16, 0, capacity)
	for _, entry := range sorted {
		encoded, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, err
		}
		block = append(block, encoded...)
	}
	return append(block, 0), nil
}

func writeProcessInput(file *os.File, input []byte) error {
	var err error
	for len(input) != 0 {
		written, writeErr := file.Write(input)
		input = input[written:]
		if writeErr != nil {
			err = writeErr
			break
		}
	}
	closeErr := closeFile(file)
	if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_NO_DATA) || errors.Is(err, os.ErrClosed) {
		err = nil
	}
	return errors.Join(err, closeErr)
}
