//go:build windows

package proctree

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsOwnerNativeTrees(t *testing.T) {
	for _, mode := range []string{"descendant", "grandchild"} {
		t.Run(mode, func(t *testing.T) {
			command := helperCommand(t, mode)
			command.CleanupTimeout = DefaultCleanupTimeout
			result, err := Run(t.Context(), command)
			if err != nil || result.Outcome != OutcomeCompleted {
				t.Fatalf("Run = (%#v, %v)", result, err)
			}
			for _, processID := range parseProcessIDs(t, result.Stdout) {
				assertProcessExited(t, processID)
			}
		})
	}
}

func TestWindowsOwnerDeadlineTerminatesTree(t *testing.T) {
	command := helperCommand(t, "block-tree")
	command.Timeout = nativeDeadlineTimeout
	result, err := Run(t.Context(), command)
	if !errors.Is(err, ErrDeadline) || errors.Is(err, ErrCleanup) || result.Outcome != OutcomeDeadline {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	for _, processID := range parseProcessIDs(t, result.Stdout) {
		assertProcessExited(t, processID)
	}
}

func TestWindowsJobKillsTreeWhenControllerExits(t *testing.T) {
	command := helperExec(t, "controller")
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	ownedTestProcess(t, nativeProcessID(t, command.Process.Pid))
	ready := make(chan checkpoint, 1)
	go readCheckpoint(output, ready)
	var observed checkpoint
	select {
	case observed = <-ready:
	case <-time.After(outputSafetyTimeout):
		t.Fatal("controller checkpoint deadline exceeded")
	}
	if observed.err != nil {
		t.Fatal(observed.err)
	}
	descendantID := parseProcessID(t, strings.TrimSpace(observed.line))
	descendant := ownedTestProcess(t, descendantID)
	if err = command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = command.Wait(); err == nil {
		t.Fatal("terminated controller returned success")
	}
	assertWindowsProcessTerminates(t, descendant, "descendant")
}

func TestWindowsNestedJobKillsTreeWhenControllerExits(t *testing.T) {
	parent := newNativeWindowsParent(t)
	closed := false
	t.Cleanup(func() {
		if !closed {
			closeNativeWindowsParent(t, parent)
		}
	})
	command, output := startNestedWindowsController(t, parent)
	descendantID := parseProcessID(t, strings.TrimSpace(readCheckpointLine(t, output)))
	descendant := ownedTestProcess(t, descendantID)
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("terminated nested controller returned success")
	}
	assertWindowsProcessTerminates(t, descendant, "nested descendant")
	closeNativeWindowsParent(t, parent)
	closed = true
}

func newNativeWindowsParent(t *testing.T) *windowsOwner {
	t.Helper()
	ownerValue, err := newWindowsOwner(systemNativeAPI())
	if err != nil {
		t.Fatal(err)
	}
	parent, ok := ownerValue.(*windowsOwner)
	if !ok {
		t.Fatalf("Windows owner type = %T", ownerValue)
	}
	return parent
}

func startNestedWindowsController(t *testing.T, parent *windowsOwner) (*exec.Cmd, io.Reader) {
	t.Helper()
	command := helperExec(t, "controller")
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err = parent.start(command, windowsCommand(command)); err != nil {
		t.Fatal(err)
	}
	stdout, ok := command.Stdout.(*os.File)
	if !ok {
		t.Fatalf("controller stdout type = %T", command.Stdout)
	}
	if err = stdout.Close(); err != nil {
		t.Fatal(err)
	}
	return command, output
}

func closeNativeWindowsParent(t *testing.T, parent *windowsOwner) {
	t.Helper()
	deadline := time.Now().Add(DefaultCleanupTimeout)
	if err := errors.Join(parent.terminate(), parent.wait(deadline), parent.close(deadline)); err != nil {
		t.Fatal(err)
	}
}

func readCheckpointLine(t *testing.T, reader io.Reader) string {
	t.Helper()
	ready := make(chan checkpoint, 1)
	go readCheckpoint(reader, ready)
	select {
	case observed := <-ready:
		if observed.err != nil {
			t.Fatal(observed.err)
		}
		return observed.line
	case <-time.After(outputSafetyTimeout):
		t.Fatal("nested controller checkpoint deadline exceeded")
	}
	return ""
}

type checkpoint struct {
	line string
	err  error
}

func readCheckpoint(reader io.Reader, ready chan<- checkpoint) {
	line, err := bufio.NewReaderSize(reader, 64).ReadString('\n')
	ready <- checkpoint{line: line, err: err}
}

func runPlatformHelper(mode string) bool {
	switch mode {
	case "descendant":
		startHelperChild("block")
	case "grandchild":
		startHelperChild("spawn-grandchild")
	case "spawn-grandchild":
		startHelperChild("block")
		blockHelper()
	case "block-tree":
		startHelperChild("block")
		blockHelper()
	case "controller":
		command := helperExecWithoutTest("block")
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		owner, err := newProcessOwner()
		if err != nil {
			writeHelper(os.Stderr, err.Error()+"\n")
			return true
		}
		if err = owner.start(command, windowsCommand(command)); err != nil {
			writeHelper(os.Stderr, err.Error()+"\n")
			return true
		}
		writeHelper(os.Stdout, strconv.Itoa(command.Process.Pid)+"\n")
		blockHelper()
	default:
		return false
	}
	return true
}

func windowsCommand(command *exec.Cmd) Command {
	return Command{
		Executable: command.Path, Arguments: command.Args[1:], Environment: command.Env,
		StdoutLimit: 1, StderrLimit: 1, CleanupTimeout: DefaultCleanupTimeout,
	}
}

func startHelperChild(mode string) {
	command := helperExecWithoutTest(mode)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		writeHelper(os.Stderr, err.Error()+"\n")
		return
	}
	writeHelper(os.Stdout, strconv.Itoa(command.Process.Pid)+"\n")
}

func helperExec(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	command := helperExecWithoutTest(mode)
	temporary := t.TempDir()
	command.Env = append(command.Env, "GOCOVERDIR="+temporary, "TEMP="+temporary, "TMP="+temporary)
	return command
}

func helperExecWithoutTest(mode string) *exec.Cmd {
	return &exec.Cmd{
		Path: os.Args[0], Args: []string{os.Args[0], "-test.run=^TestHelperFailure$", "--", mode},
		Env: append([]string{"PROCTREE_HELPER=1"}, selectedSystemEnvironment()...),
	}
}

func selectedSystemEnvironment() []string {
	result := make([]string, 0, 3)
	for _, name := range []string{"SYSTEMROOT", "TEMP", "TMP"} {
		if value, found := os.LookupEnv(name); found {
			result = append(result, name+"="+value)
		}
	}
	return result
}

func parseProcessIDs(t *testing.T, value []byte) []uint32 {
	t.Helper()
	lines := strings.Fields(string(value))
	result := make([]uint32, len(lines))
	for index, line := range lines {
		result[index] = parseProcessID(t, line)
	}
	if len(result) == 0 {
		t.Fatal("no process identifiers were retained")
	}
	return result
}

func parseProcessID(t *testing.T, value string) uint32 {
	t.Helper()
	var processID uint32
	count, err := fmt.Sscan(value, &processID)
	if err != nil || count != 1 || processID == 0 || strconv.FormatUint(uint64(processID), 10) != value {
		t.Fatalf("process identifier %q: %v", value, err)
	}
	return processID
}

func nativeProcessID(t *testing.T, processID int) uint32 {
	t.Helper()
	return parseProcessID(t, strconv.Itoa(processID))
}

func ownedTestProcess(t *testing.T, processID uint32) windows.Handle {
	t.Helper()
	process, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, processID)
	if err != nil {
		t.Fatalf("open process %d: %v", processID, err)
	}
	t.Cleanup(func() {
		defer func() {
			if closeErr := windows.CloseHandle(process); closeErr != nil {
				t.Errorf("close process %d: %v", processID, closeErr)
			}
		}()
		state, waitErr := windows.WaitForSingleObject(process, 0)
		if waitErr == nil && state != uint32(windows.WAIT_OBJECT_0) {
			if terminateErr := windows.TerminateProcess(process, 1); terminateErr != nil {
				t.Errorf("terminate process %d: %v", processID, terminateErr)
			}
			waitMilliseconds, durationErr := durationMilliseconds(DefaultCleanupTimeout)
			if durationErr != nil {
				t.Errorf("process wait duration: %v", durationErr)
				return
			}
			if _, finalWaitErr := windows.WaitForSingleObject(process, waitMilliseconds); finalWaitErr != nil {
				t.Errorf("wait for process %d: %v", processID, finalWaitErr)
			}
		}
	})
	return process
}

func assertProcessExited(t *testing.T, processID uint32) {
	t.Helper()
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, processID)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return
	}
	if err != nil {
		t.Fatalf("open process %d: %v", processID, err)
	}
	defer func() {
		if closeErr := windows.CloseHandle(process); closeErr != nil {
			t.Errorf("close process %d: %v", processID, closeErr)
		}
	}()
	if state, waitErr := windows.WaitForSingleObject(process, 0); waitErr != nil || state != uint32(windows.WAIT_OBJECT_0) {
		t.Fatalf("descendant state = %d, error %v", state, waitErr)
	}
}

func assertWindowsProcessTerminates(t *testing.T, process windows.Handle, name string) {
	t.Helper()
	waitMilliseconds, err := durationMilliseconds(DefaultCleanupTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if state, waitErr := windows.WaitForSingleObject(process, waitMilliseconds); waitErr != nil || state != uint32(windows.WAIT_OBJECT_0) {
		t.Fatalf("%s state = %d, error %v", name, state, waitErr)
	}
}
