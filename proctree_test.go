package proctree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const outputSafetyTimeout = 5 * time.Second
const nativeDeadlineTimeout = time.Second

func TestMain(m *testing.M) {
	if handled, _ := DispatchWatchdog(os.Args); handled {
		return
	}
	if os.Getenv("PROCTREE_HELPER") == "1" && os.Args[len(os.Args)-1] != "failure" {
		runHelper()
		return
	}
	m.Run()
}

func TestHelperFailure(t *testing.T) {
	if os.Getenv("PROCTREE_HELPER") == "1" && os.Args[len(os.Args)-1] == "failure" {
		t.FailNow()
	}
}

func TestAdmitCopiesValidCommand(t *testing.T) {
	t.Parallel()
	command := validCommand(t)
	admitted, err := admit(command)
	if err != nil || admitted.CleanupTimeout != DefaultCleanupTimeout {
		t.Fatalf("admit = (%#v, %v)", admitted, err)
	}
	command.Arguments[0] = "changed"
	command.Environment[0] = "VALUE=changed"
	command.Input[0] = 'x'
	if admitted.Arguments[0] == "changed" || admitted.Environment[0] == "VALUE=changed" || admitted.Input[0] == 'x' {
		t.Fatal("admitted command retained caller state")
	}
}

func TestAdmitRejectsInvalidCommands(t *testing.T) {
	t.Parallel()
	valid := validCommand(t)
	tests := []func(*Command){
		func(command *Command) { command.Executable = "relative" },
		func(command *Command) { command.Executable += "\x00" },
		func(command *Command) {
			command.Executable = filepath.Join(filepath.Dir(command.Executable), string([]byte{0xff}))
		},
		func(command *Command) {
			command.Executable = filepath.Join(filepath.Dir(command.Executable), strings.Repeat("x", MaxPathBytes+1))
		},
		func(command *Command) { command.Directory = "relative" },
		func(command *Command) {
			command.Directory = filepath.Join(filepath.Dir(command.Executable), "invalid\x00directory")
		},
		func(command *Command) {
			command.Directory = filepath.Join(filepath.Dir(command.Executable), string([]byte{0xff}))
		},
		func(command *Command) {
			command.Directory = filepath.Join(filepath.Dir(command.Executable), strings.Repeat("x", MaxPathBytes+1))
		},
		func(command *Command) { command.Arguments = make([]string, MaxArguments+1) },
		func(command *Command) { command.Arguments = []string{strings.Repeat("x", MaxArgumentBytes+1)} },
		func(command *Command) { command.Arguments = []string{"invalid\x00argument"} },
		func(command *Command) { command.Environment = make([]string, MaxEnvironment+1) },
		func(command *Command) { command.Environment = []string{"INVALID"} },
		func(command *Command) { command.Environment = []string{"=invalid"} },
		func(command *Command) { command.Environment = []string{"INVALID-NAME=value"} },
		func(command *Command) { command.Environment = []string{"VALUE=one", "VALUE=two"} },
		func(command *Command) {
			command.Environment = []string{"VALUE=" + strings.Repeat("x", MaxEnvironmentBytes)}
		},
		func(command *Command) { command.Input = make([]byte, MaxInputBytes+1) },
		func(command *Command) { command.StdoutLimit = 0 },
		func(command *Command) { command.StdoutLimit = MaxOutputBytes + 1 },
		func(command *Command) { command.StderrLimit = 0 },
		func(command *Command) { command.Timeout = -1 },
		func(command *Command) { command.Timeout = MaxTimeout + 1 },
		func(command *Command) { command.CleanupTimeout = -1 },
		func(command *Command) { command.CleanupTimeout = MaxCleanupTimeout + 1 },
	}
	for index, mutate := range tests {
		command := cloneCommand(valid)
		mutate(&command)
		if _, err := admit(command); !errors.Is(err, ErrInvalid) {
			t.Fatalf("case %d error = %v", index, err)
		}
	}
}

func TestEnvironmentNameCaseMatchesPlatform(t *testing.T) {
	t.Parallel()
	command := validCommand(t)
	command.Environment = []string{"VALUE=one", "value=two"}
	_, err := admit(command)
	if runtime.GOOS == "windows" && !errors.Is(err, ErrInvalid) {
		t.Fatalf("Windows duplicate error = %v", err)
	}
	if runtime.GOOS != "windows" && err != nil {
		t.Fatalf("Unix environment error = %v", err)
	}
}

func FuzzAdmitCommand(f *testing.F) {
	f.Add("argument", "VALUE=one", []byte("input"))
	f.Add("invalid\x00argument", "INVALID", []byte{})
	executable, err := os.Executable()
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, argument, environment string, input []byte) {
		if len(argument)+len(environment) > MaxArgumentBytes || len(input) > MaxInputBytes+1 {
			return
		}
		command := Command{
			Executable: executable, Arguments: []string{argument}, Environment: []string{environment}, Input: input,
			StdoutLimit: 1, StderrLimit: 1,
		}
		first, firstErr := admit(command)
		second, secondErr := admit(command)
		if !reflect.DeepEqual(first, second) || (firstErr == nil) != (secondErr == nil) || errors.Is(firstErr, ErrInvalid) != errors.Is(secondErr, ErrInvalid) {
			t.Fatalf("admission differs: (%#v, %v) and (%#v, %v)", first, firstErr, second, secondErr)
		}
	})
}

func TestRunBasicOutcomes(t *testing.T) {
	tests := []struct {
		mode    string
		outcome Outcome
		wantErr error
	}{
		{mode: "success", outcome: OutcomeCompleted},
		{mode: "failure", outcome: OutcomeExitFailure, wantErr: ErrExit},
	}
	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			command := helperCommand(t, test.mode)
			result, err := Run(t.Context(), command)
			if !errors.Is(err, test.wantErr) || result.Outcome != test.outcome || !result.Started {
				t.Fatalf("Run = (%#v, %v)", result, err)
			}
		})
	}
}

func TestRunPreservesCommandData(t *testing.T) {
	tests := []struct {
		mode      string
		arguments []string
		input     []byte
		environ   string
		want      string
	}{
		{mode: "arguments", arguments: []string{";", "$()", "&"}, want: ";|$()|&\n"},
		{mode: "environment", environ: "EXACT_VALUE=retained", want: "retained\n"},
		{mode: "input", input: []byte("input"), want: "input\n"},
	}
	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			command := helperCommand(t, test.mode)
			command.Arguments = append(command.Arguments[:len(command.Arguments)-1], append(test.arguments, test.mode)...)
			if test.environ != "" {
				command.Environment = append(command.Environment, test.environ)
			}
			command.Input = test.input
			result, err := Run(t.Context(), command)
			if err != nil || string(result.Stdout) != test.want {
				t.Fatalf("Run = (%#v, %v)", result, err)
			}
		})
	}
}

type outputOverflowCase struct {
	mode           string
	stdout, stderr int
	either         bool
}

func TestRunAllowsUnreadInput(t *testing.T) {
	command := helperCommand(t, "success")
	command.Input = make([]byte, MaxInputBytes)
	result, err := Run(t.Context(), command)
	if err != nil || result.Outcome != OutcomeCompleted {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
}

func TestRunTerminatesOnOutputOverflow(t *testing.T) {
	tests := []outputOverflowCase{
		{mode: "stdout-overflow", stdout: 8},
		{mode: "stderr-overflow", stderr: 8},
		{mode: "both-overflow", either: true},
	}
	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			command := helperCommand(t, test.mode)
			command.StdoutLimit, command.StderrLimit = 8, 8
			ctx, cancel := context.WithTimeout(t.Context(), outputSafetyTimeout)
			defer cancel()
			result, err := Run(ctx, command)
			assertOutputOverflowOutcome(t, result, err)
			assertOutputOverflowLengths(t, test, command, result)
		})
	}
}

func assertOutputOverflowOutcome(t *testing.T, result Result, err error) {
	t.Helper()
	if !errors.Is(err, ErrOutputLimit) || errors.Is(err, ErrDeadline) || result.Outcome != OutcomeOutputLimit {
		t.Fatalf("outcome = (%#v, %v)", result, err)
	}
}

func assertOutputOverflowLengths(t *testing.T, test outputOverflowCase, command Command, result Result) {
	t.Helper()
	if len(result.Stdout) > command.StdoutLimit || len(result.Stderr) > command.StderrLimit {
		t.Fatalf("output lengths = %d, %d", len(result.Stdout), len(result.Stderr))
	}
	if test.either {
		if len(result.Stdout) != command.StdoutLimit && len(result.Stderr) != command.StderrLimit {
			t.Fatalf("scheduled output lengths = %d, %d", len(result.Stdout), len(result.Stderr))
		}
		return
	}
	if test.stdout != 0 && len(result.Stdout) != test.stdout || test.stderr != 0 && len(result.Stderr) != test.stderr {
		t.Fatalf("output lengths = %d, %d", len(result.Stdout), len(result.Stderr))
	}
}

func TestRunRejectsNilAndCancelledContexts(t *testing.T) {
	t.Parallel()
	command := validCommand(t)
	if result, err := Run(nilContext(), command); !errors.Is(err, ErrInvalid) || result.Outcome != OutcomeInvalid {
		t.Fatalf("nil context = (%#v, %v)", result, err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := Run(cancelled, command); !errors.Is(err, ErrCancelled) || result.Started || result.Outcome != OutcomeCancelled {
		t.Fatalf("cancelled context = (%#v, %v)", result, err)
	}
	deadline, cancelDeadline := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	if result, err := Run(deadline, command); !errors.Is(err, ErrDeadline) || result.Started || result.Outcome != OutcomeDeadline {
		t.Fatalf("expired context = (%#v, %v)", result, err)
	}
}

func nilContext() context.Context { return nil }

func validCommand(t testing.TB) Command {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	temporary := t.TempDir()
	return Command{
		Executable: executable, Arguments: []string{"-test.run=^TestHelperFailure$", "--", "success"},
		Environment: []string{"PROCTREE_HELPER=1", "GOCOVERDIR=" + temporary, "TEMP=" + temporary, "TMP=" + temporary},
		Input:       []byte("input"), StdoutLimit: 1024, StderrLimit: 1024,
	}
}

func helperCommand(t testing.TB, mode string) Command {
	t.Helper()
	command := validCommand(t)
	command.Arguments[len(command.Arguments)-1] = mode
	return command
}

func cloneCommand(command Command) Command {
	command.Arguments = append([]string(nil), command.Arguments...)
	command.Environment = append([]string(nil), command.Environment...)
	command.Input = append([]byte(nil), command.Input...)
	return command
}

func runHelper() {
	arguments := os.Args
	mode := arguments[len(arguments)-1]
	if helperOutput(mode) || helperData(mode, arguments) || runPlatformHelper(mode) {
		return
	}
	if _, err := fmt.Fprintln(os.Stderr, "unknown helper mode"); err != nil {
		return
	}
}

func helperOutput(mode string) bool {
	switch mode {
	case "success":
		return writeHelper(os.Stdout, "stdout\n") && writeHelper(os.Stderr, "stderr\n")
	case "stdout-overflow":
		return writeThenBlock(os.Stdout)
	case "stderr-overflow":
		return writeThenBlock(os.Stderr)
	case "both-overflow":
		if !writeHelper(os.Stdout, strings.Repeat("x", 64)) || !writeHelper(os.Stderr, strings.Repeat("x", 64)) {
			return true
		}
		<-make(chan struct{})
	}
	return false
}

func helperData(mode string, arguments []string) bool {
	switch mode {
	case "arguments":
		return writeHelper(os.Stdout, strings.Join(arguments[len(arguments)-4:len(arguments)-1], "|")+"\n")
	case "environment":
		return writeHelper(os.Stdout, os.Getenv("EXACT_VALUE")+"\n")
	case "input":
		value := make([]byte, len("input"))
		if _, err := io.ReadFull(os.Stdin, value); err != nil {
			return true
		}
		return writeHelper(os.Stdout, string(value)+"\n")
	case "block":
		blockHelper()
	}
	return false
}

func writeThenBlock(writer io.Writer) bool {
	if !writeHelper(writer, strings.Repeat("x", 64)) {
		return true
	}
	blockHelper()
	return true
}

func writeHelper(writer io.Writer, value string) bool {
	_, err := io.WriteString(writer, value)
	return err == nil
}

func blockHelper() {
	read, write, err := os.Pipe()
	if err != nil {
		return
	}
	var value [1]byte
	_, readErr := read.Read(value[:])
	closeErr := errors.Join(read.Close(), write.Close())
	if err = errors.Join(readErr, closeErr); err != nil {
		writeHelper(os.Stderr, err.Error()+"\n")
	}
}
