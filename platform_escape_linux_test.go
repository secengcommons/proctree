//go:build linux

package proctree

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const escapedOuterRoot = "PROCTREE_ESCAPE_OUTER_ROOT"
const escapedMiddleRoot = "PROCTREE_ESCAPE_MIDDLE_ROOT"
const escapedControlPath = "PROCTREE_ESCAPE_CONTROL"
const escapedCheckpointPath = "PROCTREE_ESCAPE_CHECKPOINT"

func TestLinuxPID1ReportsCleanupFailure(t *testing.T) {
	if os.Getenv("PROCTREE_PID1_QUALIFICATION") == "" {
		return
	}
	if os.Getpid() != 1 {
		t.Fatalf("PID 1 qualification process = %d", os.Getpid())
	}
	command := helperCommand(t, "descendant")
	command.CleanupTimeout = nativeDeadlineTimeout
	result, err := Run(t.Context(), command)
	if !errors.Is(err, ErrCleanup) || result.Outcome != OutcomeCleanupFailure {
		t.Fatalf("PID 1 Run = (%#v, %v)", result, err)
	}
}

func TestLinuxEscapedWriterDoesNotExtendCleanup(t *testing.T) {
	root := t.TempDir()
	lifetime, command := prepareEscapedWriterRun(t, root, nativeDeadlineTimeout)
	t.Cleanup(func() {
		if err := closeFile(lifetime); err != nil {
			t.Error(err)
		}
	})
	result, err := Run(t.Context(), command)
	processes := awaitEscapedProcesses(t, filepath.Join(root, "checkpoint"))
	retainEscapedFallback(t, processes)
	if !errors.Is(err, ErrCleanup) || !result.Started || result.Outcome != OutcomeCleanupFailure ||
		result.Stdout != nil || result.Stderr != nil {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	if err := lifetime.Close(); err != nil {
		t.Fatal(err)
	}
	awaitEscapedExit(t, processes)
}

func TestLinuxEscapedWriterDiesWithTestRunner(t *testing.T) {
	root := t.TempDir()
	command := interruptionRunnerCommand(t, "TestLinuxEscapedWriterInterruptionFixture", escapedOuterRoot+"="+root)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	runnerJoined := false
	t.Cleanup(func() {
		if !runnerJoined {
			stopTestRunner(t, command)
		}
	})
	processes := awaitEscapedProcesses(t, filepath.Join(root, "checkpoint"))
	retainEscapedFallback(t, processes)
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("interrupted test runner returned success")
	}
	runnerJoined = true
	awaitEscapedExit(t, processes)
}

func TestLinuxEscapedWriterDiesWithOuterRunner(t *testing.T) {
	root := t.TempDir()
	command := interruptionRunnerCommand(t, "TestLinuxEscapedWriterOuterFixture", escapedMiddleRoot+"="+root)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	runnerJoined := false
	t.Cleanup(func() {
		if !runnerJoined {
			stopTestRunner(t, command)
		}
	})
	processes := awaitOuterProcesses(t, filepath.Join(root, "outer-checkpoint"))
	retainProcessFallback(t, processes[:])
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("outer fixture runner returned success")
	}
	runnerJoined = true
	for _, processID := range processes {
		if err := waitTestProcessExit(processID, time.Now().Add(outputSafetyTimeout)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLinuxEscapedWriterInterruptionFixture(t *testing.T) {
	root := os.Getenv(escapedOuterRoot)
	if root == "" {
		return
	}
	lifetime, command := prepareEscapedWriterRun(t, root, MaxCleanupTimeout)
	t.Cleanup(func() {
		if err := closeFile(lifetime); err != nil {
			t.Error(err)
		}
	})
	result, err := Run(t.Context(), command)
	t.Fatalf("escaped fixture completed before interruption: (%#v, %v)", result, err)
}

func TestLinuxEscapedWriterOuterFixture(t *testing.T) {
	root := os.Getenv(escapedMiddleRoot)
	if root == "" {
		return
	}
	inner := interruptionRunnerCommand(t, "TestLinuxEscapedWriterInterruptionFixture", escapedOuterRoot+"="+root)
	if err := inner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopTestRunner(t, inner)
	})
	processes := awaitEscapedProcesses(t, filepath.Join(root, "checkpoint"))
	checkpoint := fmt.Sprintf("%d %d %d\n", inner.Process.Pid, processes[0], processes[1])
	if err := os.WriteFile(filepath.Join(root, "outer-checkpoint"), []byte(checkpoint), 0o600); err != nil { //nolint:gosec // root is an owned test directory
		t.Fatal(err)
	}
	blockHelper()
}

func interruptionRunnerCommand(t *testing.T, testName, environment string) *exec.Cmd {
	t.Helper()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^"+testName+"$") //nolint:gosec // The fixed test harness selects testName
	command.Env = append(os.Environ(), environment)
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	return command
}

func stopTestRunner(t *testing.T, command *exec.Cmd) {
	t.Helper()
	killErr := command.Process.Kill()
	waitErr := command.Wait()
	if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		t.Errorf("stop test runner: %v", killErr)
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		t.Errorf("join test runner: %v", waitErr)
	}
}

func prepareEscapedWriterRun(t *testing.T, root string, cleanupTimeout time.Duration) (*os.File, Command) {
	t.Helper()
	control := filepath.Join(root, "lifetime")
	if err := unix.Mkfifo(control, 0o600); err != nil {
		t.Fatal(err)
	}
	lifetime, err := os.OpenFile(control, os.O_RDWR, 0) //nolint:gosec // control is joined beneath the owned test root
	if err != nil {
		t.Fatal(err)
	}
	command := escapedWriterCommand(t, root, cleanupTimeout)
	return lifetime, command
}

func escapedWriterCommand(t *testing.T, root string, cleanupTimeout time.Duration) Command {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Command{
		Executable: executable,
		Arguments:  []string{"-test.run=^TestHelperFailure$", "--", "escape-writer"},
		Environment: []string{
			"PROCTREE_HELPER=1", "GOCOVERDIR=" + root, "TEMP=" + root, "TMP=" + root,
			escapedControlPath + "=" + filepath.Join(root, "lifetime"),
			escapedCheckpointPath + "=" + filepath.Join(root, "checkpoint"),
		},
		StdoutLimit: 1024, StderrLimit: 1024, CleanupTimeout: cleanupTimeout,
	}
}

func awaitEscapedProcesses(t *testing.T, path string) [2]int {
	t.Helper()
	deadline := time.NewTimer(outputSafetyTimeout)
	defer deadline.Stop()
	retry := time.NewTicker(failedTerminationDeadline)
	defer retry.Stop()
	for {
		encoded, readErr := os.ReadFile(path) //nolint:gosec // path is an owned test checkpoint
		fields := strings.Fields(string(encoded))
		if readErr == nil && len(fields) == 2 {
			supervisor, supervisorErr := strconv.Atoi(fields[0])
			child, childErr := strconv.Atoi(fields[1])
			if supervisorErr == nil && childErr == nil && supervisor > 0 && child > 0 && processAlive(supervisor) && processAlive(child) {
				return [2]int{supervisor, child}
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("escaped process checkpoint = (%q, %v)", encoded, readErr)
		case <-retry.C:
		}
	}
}

func retainEscapedFallback(t *testing.T, processes [2]int) {
	t.Helper()
	retainProcessFallback(t, processes[:])
}

func retainProcessFallback(t *testing.T, processes []int) {
	t.Helper()
	t.Cleanup(func() {
		for _, processID := range processes {
			if processAlive(processID) {
				if err := stopTestProcess(processID); err != nil {
					t.Errorf("stop escaped process %d: %v", processID, err)
				}
			}
		}
	})
}

func awaitOuterProcesses(t *testing.T, path string) [3]int {
	t.Helper()
	deadline := time.NewTimer(outputSafetyTimeout)
	defer deadline.Stop()
	retry := time.NewTicker(failedTerminationDeadline)
	defer retry.Stop()
	for {
		encoded, readErr := os.ReadFile(path) //nolint:gosec // path is an owned test checkpoint
		fields := strings.Fields(string(encoded))
		if readErr == nil && len(fields) == 3 {
			var result [3]int
			valid := true
			for index, field := range fields {
				processID, parseErr := strconv.Atoi(field)
				result[index] = processID
				valid = valid && parseErr == nil && processID > 0 && processAlive(processID)
			}
			if valid {
				return result
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("outer process checkpoint = (%q, %v)", encoded, readErr)
		case <-retry.C:
		}
	}
}

func awaitEscapedExit(t *testing.T, processes [2]int) {
	t.Helper()
	for _, processID := range processes {
		if err := waitTestProcessExit(processID, time.Now().Add(outputSafetyTimeout)); err != nil {
			t.Fatal(err)
		}
	}
}

func runEscapedWriterHelper(mode string) bool {
	switch mode {
	case "escape-writer":
		startEscapedSupervisor()
	case "escape-supervisor":
		runEscapedSupervisor()
	default:
		return false
	}
	return true
}

func startEscapedSupervisor() {
	command := unixHelperCommand("escape-supervisor")
	command.Env = append(command.Env,
		escapedControlPath+"="+os.Getenv(escapedControlPath),
		escapedCheckpointPath+"="+os.Getenv(escapedCheckpointPath),
	)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		writeHelper(os.Stderr, err.Error()+"\n")
	}
}

func runEscapedSupervisor() {
	lifetime, err := os.Open(os.Getenv(escapedControlPath)) //nolint:gosec // the test parent supplies the lifetime path
	if err != nil {
		writeHelper(os.Stderr, err.Error()+"\n")
		return
	}
	child := unixHelperCommand("block")
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err = child.Start(); err != nil {
		writeHelper(os.Stderr, errors.Join(err, lifetime.Close()).Error()+"\n")
		return
	}
	checkpoint := fmt.Sprintf("%d %d\n", os.Getpid(), child.Process.Pid)
	if err = os.WriteFile(os.Getenv(escapedCheckpointPath), []byte(checkpoint), 0o600); err != nil { //nolint:gosec // the test parent supplies the checkpoint path
		writeHelper(os.Stderr, err.Error()+"\n")
	}
	_, drainErr := io.Copy(io.Discard, lifetime)
	closeErr := lifetime.Close()
	killErr := child.Process.Kill()
	waitErr := child.Wait()
	if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok && exitErr != nil {
		waitErr = nil
	}
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	if err = errors.Join(drainErr, closeErr, killErr, waitErr); err != nil {
		writeHelper(os.Stderr, err.Error()+"\n")
	}
}
