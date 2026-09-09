//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

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
	"syscall"
	"testing"
	"time"
)

func TestUnixOwnerNativeTrees(t *testing.T) {
	for _, mode := range []string{"descendant", "grandchild"} {
		t.Run(mode, func(t *testing.T) {
			command := helperCommand(t, mode)
			command.CleanupTimeout = DefaultCleanupTimeout
			result, err := Run(t.Context(), command)
			if err != nil || result.Outcome != OutcomeCompleted {
				t.Fatalf("Run = (%#v, %v)", result, err)
			}
			for _, processID := range parseUnixProcessIDs(t, result.Stdout) {
				if processAlive(processID) {
					t.Fatalf("process %d remained alive", processID)
				}
			}
		})
	}
}

func TestUnixOwnerDeadlineTerminatesTree(t *testing.T) {
	command := helperCommand(t, "block-tree")
	command.Timeout = nativeDeadlineTimeout
	result, err := Run(t.Context(), command)
	if !errors.Is(err, ErrDeadline) || errors.Is(err, ErrCleanup) || result.Outcome != OutcomeDeadline {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	for _, processID := range parseUnixProcessIDs(t, result.Stdout) {
		if processAlive(processID) {
			t.Fatalf("process %d remained alive", processID)
		}
	}
}

func TestUnixOwnerDetectsWatchdogLoss(t *testing.T) {
	owner := &unixOwner{api: systemUnixAPI()}
	factory := func() (processOwner, error) { return &watchdogKillingOwner{unixOwner: owner}, nil }
	result, err := run(t.Context(), helperCommand(t, "block"), factory)
	if !errors.Is(err, ErrOwnership) || errors.Is(err, ErrCleanup) || result.Outcome != OutcomeOwnershipFailure || !result.Started {
		t.Fatalf("run = (%#v, %v)", result, err)
	}
}

type watchdogKillingOwner struct{ *unixOwner }

func (owner *watchdogKillingOwner) start(command *exec.Cmd, admitted Command) error {
	if err := owner.unixOwner.start(command, admitted); err != nil {
		return err
	}
	return syscall.Kill(owner.pid, syscall.SIGKILL)
}

func TestUnixWatchdogKillsTreeWhenControllerExits(t *testing.T) {
	fixture := newUnixControllerFixture(t)
	fixture.readDescendant(t)
	fixture.terminateController(t)
	fixture.awaitDescendant(t)
}

type unixControllerFixture struct {
	command          *exec.Cmd
	output           io.ReadCloser
	sentinel         *os.File
	controllerJoined bool
	descendantExited bool
}

func newUnixControllerFixture(t *testing.T) *unixControllerFixture {
	t.Helper()
	sentinelRead, sentinelWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := errors.Join(closeFile(sentinelRead), closeFile(sentinelWrite)); closeErr != nil {
			t.Errorf("close sentinel pipe: %v", closeErr)
		}
	})
	command := unixHelperCommand("controller")
	command.ExtraFiles = []*os.File{sentinelWrite}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	outputFile, ok := output.(*os.File)
	if !ok {
		t.Fatal("controller output is not a file")
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	fixture := &unixControllerFixture{command: command, output: output, sentinel: sentinelRead}
	t.Cleanup(func() {
		if !fixture.controllerJoined {
			killErr := command.Process.Kill()
			waitErr := command.Wait()
			if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				t.Errorf("stop controller: %v", killErr)
			}
			if waitErr == nil {
				t.Error("stopped controller returned success")
			}
		}
	})
	if err = sentinelWrite.Close(); err != nil {
		t.Fatal(err)
	}
	if err = outputFile.SetReadDeadline(time.Now().Add(outputSafetyTimeout)); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *unixControllerFixture) readDescendant(t *testing.T) {
	t.Helper()
	line, err := bufio.NewReaderSize(fixture.output, 64).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	descendantID, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || descendantID <= 0 {
		t.Fatalf("descendant identifier %q: %v", line, err)
	}
	t.Cleanup(func() {
		if !fixture.descendantExited {
			if cleanupErr := stopTestProcess(descendantID); cleanupErr != nil {
				t.Errorf("stop descendant: %v", cleanupErr)
			}
		}
	})
}

func (fixture *unixControllerFixture) terminateController(t *testing.T) {
	t.Helper()
	if err := fixture.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.command.Wait(); err == nil {
		t.Fatal("terminated controller returned success")
	}
	fixture.controllerJoined = true
}

func (fixture *unixControllerFixture) awaitDescendant(t *testing.T) {
	t.Helper()
	if err := fixture.sentinel.SetReadDeadline(time.Now().Add(outputSafetyTimeout)); err != nil {
		t.Fatal(err)
	}
	var marker [1]byte
	if count, readErr := fixture.sentinel.Read(marker[:]); count != 0 || !errors.Is(readErr, io.EOF) {
		t.Fatalf("descendant sentinel = (%d, %v)", count, readErr)
	}
	fixture.descendantExited = true
}

func TestUnixDispatchRejectsOrdinaryArguments(t *testing.T) {
	if handled, code := DispatchWatchdog([]string{"proctree"}); handled || code != 0 {
		t.Fatalf("dispatch = (%t, %d)", handled, code)
	}
}

func runPlatformHelper(mode string) bool {
	switch mode {
	case "descendant":
		startUnixHelperChild("block")
	case "grandchild":
		startUnixHelperChild("spawn-grandchild")
	case "spawn-grandchild":
		startUnixHelperChild("block")
		blockHelper()
	case "block-tree":
		startUnixHelperChild("block")
		blockHelper()
	case "controller":
		sentinel := os.NewFile(3, "proctree-test-sentinel")
		if sentinel == nil {
			writeHelper(os.Stderr, "sentinel descriptor is unavailable\n")
			return true
		}
		command := unixHelperCommand("block")
		command.ExtraFiles = []*os.File{sentinel}
		owner, err := newProcessOwner()
		if err != nil {
			writeHelper(os.Stderr, err.Error()+"\n")
			return true
		}
		if err = owner.start(command, cleanupCommand()); err != nil {
			writeHelper(os.Stderr, err.Error()+"\n")
			return true
		}
		writeHelper(os.Stdout, strconv.Itoa(command.Process.Pid)+"\n")
		if err = sentinel.Close(); err != nil {
			writeHelper(os.Stderr, err.Error()+"\n")
			return true
		}
		blockHelper()
	default:
		return runEscapedWriterHelper(mode)
	}
	return true
}

func startUnixHelperChild(mode string) {
	command := unixHelperCommand(mode)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		writeHelper(os.Stderr, err.Error()+"\n")
		return
	}
	writeHelper(os.Stdout, strconv.Itoa(command.Process.Pid)+"\n")
}

func unixHelperCommand(mode string) *exec.Cmd {
	return &exec.Cmd{
		Path: os.Args[0], Args: []string{os.Args[0], "-test.run=^TestHelperFailure$", "--", mode},
		Env: []string{"PROCTREE_HELPER=1"},
	}
}

func parseUnixProcessIDs(t *testing.T, value []byte) []int {
	t.Helper()
	fields := strings.Fields(string(value))
	result := make([]int, len(fields))
	for index, field := range fields {
		processID, err := strconv.Atoi(field)
		if err != nil || processID <= 0 {
			t.Fatalf("process identifier %q: %v", field, err)
		}
		result[index] = processID
	}
	if len(result) == 0 {
		t.Fatal("no process identifiers were retained")
	}
	return result
}

func processAlive(processID int) bool {
	alive, err := testProcessAlive(processID)
	return err != nil || alive
}

func stopTestProcess(processID int) error {
	err := syscall.Kill(processID, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	return waitTestProcessExit(processID, time.Now().Add(outputSafetyTimeout))
}

func waitTestProcessExit(processID int, deadline time.Time) error {
	for {
		alive, err := testProcessAlive(processID)
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("process %d did not terminate", processID)
		}
		timer := time.NewTimer(min(failedTerminationDeadline, time.Until(deadline)))
		<-timer.C
	}
}
