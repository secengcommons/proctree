package proctree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

const failedTerminationDeadline = 10 * time.Millisecond

func TestRunReportsOwnerFailures(t *testing.T) {
	t.Parallel()
	command := validCommand(t)
	tests := []struct {
		name    string
		factory ownerFactory
		outcome Outcome
		want    error
	}{
		{name: "unsupported", factory: func() (processOwner, error) { return nil, ErrUnsupported }, outcome: OutcomeUnsupported, want: ErrUnsupported},
		{name: "ownership", factory: func() (processOwner, error) { return nil, ErrOwnership }, outcome: OutcomeOwnershipFailure, want: ErrOwnership},
		{name: "ownership cleanup", factory: func() (processOwner, error) { return nil, errors.Join(ErrOwnership, ErrCleanup) }, outcome: OutcomeCleanupFailure, want: ErrCleanup},
		{name: "start", factory: ownerWith(&fakeOwner{startErr: ErrStart}), outcome: OutcomeStartFailure, want: ErrStart},
		{name: "start cleanup", factory: ownerWith(&fakeOwner{startErr: ErrStart, closeErr: errors.New("close")}), outcome: OutcomeCleanupFailure, want: ErrCleanup},
		{name: "owned start cleanup", factory: ownerWith(&fakeOwner{startErr: errors.Join(ErrOwnership, ErrCleanup)}), outcome: OutcomeCleanupFailure, want: ErrCleanup},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := run(t.Context(), command, test.factory)
			if !errors.Is(err, test.want) || result.Outcome != test.outcome || result.Started {
				t.Fatalf("run = (%#v, %v)", result, err)
			}
		})
	}
}

func TestRunRejectsInvalidCommand(t *testing.T) {
	t.Parallel()
	result, err := Run(t.Context(), Command{})
	if !errors.Is(err, ErrInvalid) || result.Outcome != OutcomeInvalid || result.ExitCode != -1 {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
}

func TestRunDoesNotStartAfterCancellationDuringOwnerCreation(t *testing.T) {
	command := validCommand(t)
	ctx, cancel := context.WithCancel(t.Context())
	owner := &fakeOwner{}
	factory := func() (processOwner, error) {
		cancel()
		return owner, nil
	}
	result, err := run(ctx, command, factory)
	if !errors.Is(err, ErrCancelled) || result.Outcome != OutcomeCancelled || result.Started || owner.command != nil {
		t.Fatalf("run = (%#v, %v)", result, err)
	}
}

func TestRunRetainsCleanupFailureAfterCancellationDuringOwnerCreation(t *testing.T) {
	command := validCommand(t)
	ctx, cancel := context.WithCancel(t.Context())
	cleanupErr := errors.New("close")
	factory := func() (processOwner, error) {
		cancel()
		return &fakeOwner{closeErr: cleanupErr}, nil
	}
	result, err := run(ctx, command, factory)
	if !errors.Is(err, ErrCancelled) || !errors.Is(err, ErrCleanup) || !errors.Is(err, cleanupErr) || result.Outcome != OutcomeCleanupFailure {
		t.Fatalf("run = (%#v, %v)", result, err)
	}
}

func TestPrepareOwnerClassifiesCaptureCleanupFailure(t *testing.T) {
	captureErr := errors.New("capture")
	capture := &outputCapture{
		stdout: capturedStream{done: make(chan error, 1)},
		stderr: capturedStream{done: make(chan error, 1)},
	}
	capture.stdout.done <- captureErr
	capture.stderr.done <- nil
	factory := func() (processOwner, error) { return nil, ErrOwnership }
	_, result, err, finished := prepareOwner(t.Context(), validCommand(t), factory, capture)
	if !finished || result.Outcome != OutcomeCleanupFailure || !errors.Is(err, ErrOwnership) || !errors.Is(err, ErrCleanup) || !errors.Is(err, captureErr) {
		t.Fatalf("prepareOwner = (%#v, %v, %t)", result, err, finished)
	}
}

func TestRunReportsCleanupFailure(t *testing.T) {
	t.Parallel()
	command := helperCommand(t, "success")
	cleanupErr := errors.New("cleanup")
	owner := &fakeOwner{terminateErr: cleanupErr, waitErr: cleanupErr, closeErr: cleanupErr}
	result, err := run(t.Context(), command, ownerWith(owner))
	if !errors.Is(err, ErrCleanup) || !errors.Is(err, cleanupErr) || result.Outcome != OutcomeCleanupFailure || !result.Started {
		t.Fatalf("run = (%#v, %v)", result, err)
	}
}

func TestRunCancellationRetainsEarlyTerminationFailure(t *testing.T) {
	command := helperCommand(t, "block")
	ctx, cancel := context.WithCancel(t.Context())
	terminationErr := errors.New("terminate")
	owner := &fakeOwner{afterStart: cancel, terminateErr: terminationErr}
	result, err := run(ctx, command, ownerWith(owner))
	if !errors.Is(err, ErrCleanup) || !errors.Is(err, terminationErr) || result.Outcome != OutcomeCleanupFailure {
		t.Fatalf("run = (%#v, %v)", result, err)
	}
}

func TestAwaitPrioritisesObservedCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	waited := make(chan error, 1)
	waited <- nil
	result := await(ctx, make(chan struct{}), &fakeOwner{}, waited)
	if !errors.Is(result.primaryErr, ErrCancelled) || result.outcome != OutcomeCancelled || result.joined {
		t.Fatalf("await = %#v", result)
	}
}

func TestAwaitRechecksCancellationAfterCompletionSelection(t *testing.T) {
	ctx := newStagedCancellationContext(t.Context())
	waited := make(chan error, 1)
	waited <- nil
	result := await(ctx, make(chan struct{}), &fakeOwner{}, waited)
	if !errors.Is(result.primaryErr, ErrCancelled) || result.outcome != OutcomeCancelled || !result.joined {
		t.Fatalf("await = %#v", result)
	}
}

func TestAwaitRechecksCancellationAfterOverflowSelection(t *testing.T) {
	ctx := newStagedCancellationContext(t.Context())
	overflow := make(chan struct{})
	close(overflow)
	result := await(ctx, overflow, &fakeOwner{}, make(chan error))
	if !errors.Is(result.primaryErr, ErrCancelled) || result.outcome != OutcomeCancelled {
		t.Fatalf("await = %#v", result)
	}
}

func TestAwaitReportsOwnerHealthFailure(t *testing.T) {
	done := make(chan struct{})
	close(done)
	owner := &failedHealthOwner{done: done, err: ErrOwnership}
	result := await(t.Context(), make(chan struct{}), owner, make(chan error))
	if !errors.Is(result.primaryErr, ErrOwnership) || result.outcome != OutcomeOwnershipFailure {
		t.Fatalf("await = %#v", result)
	}
}

func TestAwaitObservesHealthWhileWaiting(t *testing.T) {
	owner := newStagedHealthOwner(ErrOwnership, 2)
	result := await(t.Context(), make(chan struct{}), owner, make(chan error))
	if !errors.Is(result.primaryErr, ErrOwnership) || result.outcome != OutcomeOwnershipFailure {
		t.Fatalf("await = %#v", result)
	}
}

func TestAwaitPrioritisesCancellationDuringHealthSelection(t *testing.T) {
	owner := newStagedHealthOwner(ErrOwnership, 2)
	result := await(newStagedCancellationContext(t.Context()), make(chan struct{}), owner, make(chan error))
	if !errors.Is(result.primaryErr, ErrCancelled) || result.outcome != OutcomeCancelled {
		t.Fatalf("await = %#v", result)
	}
}

func TestAwaitPrioritisesHealthOverCompletionAndOverflow(t *testing.T) {
	for name, ready := range map[string]func() (<-chan struct{}, <-chan error){
		"completion": func() (<-chan struct{}, <-chan error) {
			waited := make(chan error, 1)
			waited <- nil
			return make(chan struct{}), waited
		},
		"overflow": func() (<-chan struct{}, <-chan error) {
			overflow := make(chan struct{})
			close(overflow)
			return overflow, make(chan error)
		},
	} {
		t.Run(name, func(t *testing.T) {
			overflow, waited := ready()
			owner := newStagedHealthOwner(ErrOwnership, 3)
			result := await(t.Context(), overflow, owner, waited)
			if !errors.Is(result.primaryErr, ErrOwnership) || result.outcome != OutcomeOwnershipFailure {
				t.Fatalf("await = %#v", result)
			}
		})
	}
}

func TestAwaitPrioritisesCancellationOverHealthFailure(t *testing.T) {
	done := make(chan struct{})
	close(done)
	owner := &failedHealthOwner{done: done, err: ErrOwnership}
	result := await(newStagedCancellationContext(t.Context()), make(chan struct{}), owner, make(chan error))
	if !errors.Is(result.primaryErr, ErrCancelled) || !errors.Is(result.primaryErr, ErrOwnership) || result.outcome != OutcomeCancelled {
		t.Fatalf("await = %#v", result)
	}
}

func TestExecuteCancellationConsumesWatchdogLoss(t *testing.T) {
	for name, readyAt := range map[string]int{"pre-check": 1, "selected-health": 2} {
		t.Run(name, func(t *testing.T) {
			command, admitErr := admit(validCommand(t))
			if admitErr != nil {
				t.Fatal(admitErr)
			}
			ctx := newStagedCancellationContextAt(t.Context(), 3)
			owner := newHealthCancellationOwner(readyAt)
			result, err := executeOwnedWithWait(ctx, command, ownerWith(owner), func(*exec.Cmd) error {
				<-owner.released
				return nil
			})
			if result.Outcome != OutcomeCancelled || !errors.Is(err, ErrCancelled) || !errors.Is(err, ErrOwnership) || errors.Is(err, ErrCleanup) {
				t.Fatalf("executeOwnedWithWait = (%#v, %v)", result, err)
			}
			if !owner.healthSeen {
				t.Fatal("watchdog loss was not consumed")
			}
		})
	}
}

func TestApplyOutputLimitPreservesSelectedOutcome(t *testing.T) {
	tests := []struct {
		name    string
		initial Outcome
		want    Outcome
	}{
		{name: "unselected", want: OutcomeOutputLimit},
		{name: "cancelled", initial: OutcomeCancelled, want: OutcomeCancelled},
		{name: "deadline", initial: OutcomeDeadline, want: OutcomeDeadline},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := executionWait{outcome: test.initial}
			applyOutputLimit(&result)
			if result.outcome != test.want || !errors.Is(result.primaryErr, ErrOutputLimit) {
				t.Fatalf("output limit = %#v", result)
			}
		})
	}
}

func TestExecutePreservesCancellationOutcomeDuringOverflow(t *testing.T) {
	command, err := admit(validCommand(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	create := func(stdoutLimit, stderrLimit int, overflow chan struct{}, once *sync.Once) (*outputCapture, error) {
		capture, captureErr := newOutputCapture(stdoutLimit, stderrLimit, overflow, once)
		if captureErr == nil {
			capture.stdout.buffer.full = true
			close(overflow)
		}
		return capture, captureErr
	}
	result, runErr := executeOwnedWithCapture(
		ctx, command, ownerWith(&cancellingOwner{cancel: cancel}), func(*exec.Cmd) error { return nil }, create,
	)
	if !errors.Is(runErr, ErrCancelled) || !errors.Is(runErr, ErrOutputLimit) || result.Outcome != OutcomeCancelled {
		t.Fatalf("executeOwnedWithCapture = (%#v, %v)", result, runErr)
	}
}

func TestStartFailureRetainsEstablishedCleanupDeadline(t *testing.T) {
	deadline := time.Unix(1, 0)
	owner := &deadlineOwner{}
	capture := &outputCapture{
		stdout: capturedStream{done: make(chan error, 1)},
		stderr: capturedStream{done: make(chan error, 1)},
	}
	capture.stdout.done <- nil
	capture.stderr.done <- nil
	startErr := withCleanupDeadline(ErrStart, deadline)
	if startErr.Error() != ErrStart.Error() {
		t.Fatalf("bounded start error = %q", startErr)
	}
	result, err := finishStartFailure(owner, capture, DefaultCleanupTimeout, startErr)
	if !errors.Is(err, ErrStart) || result.Outcome != OutcomeStartFailure || !owner.closeDeadline.Equal(deadline) {
		t.Fatalf("finishStartFailure = (%#v, %v, %s)", result, err, owner.closeDeadline)
	}
}

func TestCleanupOwnerRetainsAbsoluteDeadline(t *testing.T) {
	deadline := time.Unix(2, 0)
	owner := &deadlineOwner{}
	if err := cleanupOwner(owner, deadline); err != nil || !owner.waitDeadline.Equal(deadline) || !owner.closeDeadline.Equal(deadline) {
		t.Fatalf("cleanupOwner = (%v, %s, %s)", err, owner.waitDeadline, owner.closeDeadline)
	}
}

type stagedCancellationContext struct {
	context.Context
	done     chan struct{}
	checks   int
	cancelAt int
}

func newStagedCancellationContext(parent context.Context) *stagedCancellationContext {
	return newStagedCancellationContextAt(parent, 2)
}

func newStagedCancellationContextAt(parent context.Context, cancelAt int) *stagedCancellationContext {
	return &stagedCancellationContext{Context: parent, done: make(chan struct{}), cancelAt: cancelAt}
}

func (ctx *stagedCancellationContext) Done() <-chan struct{} { return ctx.done }

func (ctx *stagedCancellationContext) Err() error {
	ctx.checks++
	if ctx.checks < ctx.cancelAt {
		return nil
	}
	return context.Canceled
}

func TestRunBoundsFailedTermination(t *testing.T) {
	command := helperCommand(t, "block")
	command.CleanupTimeout = failedTerminationDeadline
	ctx, cancel := context.WithCancel(t.Context())
	owner := &fakeOwner{afterStart: cancel, skipFirstKill: true, terminateErr: errors.New("terminate")}
	result, err := run(ctx, command, ownerWith(owner))
	if !errors.Is(err, ErrCleanup) || result.Outcome != OutcomeCleanupFailure {
		t.Fatalf("run = (%#v, %v)", result, err)
	}
}

func TestExecuteOmitsUnjoinedState(t *testing.T) {
	command := validCommand(t)
	command.CleanupTimeout = time.Nanosecond
	ctx, cancel := context.WithCancel(t.Context())
	owner := &blockedOwner{afterStart: cancel}
	release := make(chan struct{})
	waitDone := make(chan struct{})
	waiter := func(*exec.Cmd) error {
		defer close(waitDone)
		<-release
		return errors.New("released")
	}
	result, err := executeOwnedWithWait(ctx, command, ownerWith(owner), waiter)
	close(release)
	<-waitDone
	if !errors.Is(err, ErrCleanup) || result.Outcome != OutcomeCleanupFailure || result.ExitCode != -1 || result.Stdout != nil || result.Stderr != nil {
		t.Fatalf("executeOwnedWithWait = (%#v, %v)", result, err)
	}
}

func TestExecuteBoundsCaptureWhenProcessCannotBeJoined(t *testing.T) {
	command := helperCommand(t, "block")
	command.CleanupTimeout = failedTerminationDeadline
	ctx, cancel := context.WithCancel(t.Context())
	owner := &unjoinedOwner{afterStart: cancel, started: make(chan struct{})}
	waitDone := make(chan struct{})
	waiter := func(command *exec.Cmd) error {
		defer close(waitDone)
		return command.Wait()
	}
	finished := make(chan executionResult, 1)
	go func() {
		result, err := executeOwnedWithWait(ctx, command, ownerWith(owner), waiter)
		finished <- executionResult{result: result, err: err}
	}()
	got := awaitUnjoinedExecution(t, owner, finished, waitDone)
	terminateUnjoinedFixture(t, owner, waitDone)
	if !errors.Is(got.err, ErrCleanup) || got.result.Outcome != OutcomeCleanupFailure || got.result.Stdout != nil || got.result.Stderr != nil {
		t.Fatalf("executeOwnedWithWait = (%#v, %v)", got.result, got.err)
	}
}

func TestExecuteBoundsCaptureAfterJoinedProcess(t *testing.T) {
	command, err := admit(validCommand(t))
	if err != nil {
		t.Fatal(err)
	}
	command.CleanupTimeout = failedTerminationDeadline
	release := make(chan struct{})
	heldClosed := make(chan error, 1)
	var createdCapture *outputCapture
	create := func(int, int, chan struct{}, *sync.Once) (*outputCapture, error) {
		stdoutRead, stdoutHeld, pipeErr := capturePipe()
		if pipeErr != nil {
			return nil, pipeErr
		}
		stderrRead, stderrHeld, pipeErr := capturePipe()
		if pipeErr != nil {
			return nil, errors.Join(pipeErr, closeFile(stdoutRead), closeFile(stdoutHeld))
		}
		capture := &outputCapture{
			stdout: capturedStream{read: stdoutRead, done: make(chan error, 1)},
			stderr: capturedStream{read: stderrRead, done: make(chan error, 1)},
		}
		createdCapture = capture
		go func() {
			<-release
			heldClosed <- errors.Join(stdoutHeld.Close(), stderrHeld.Close())
		}()
		return capture, nil
	}
	finished := make(chan executionResult, 1)
	go func() {
		result, runErr := executeOwnedWithCapture(
			t.Context(), command, ownerWith(&passiveOwner{}), func(*exec.Cmd) error { return nil }, create,
		)
		finished <- executionResult{result: result, err: runErr}
	}()
	timer := time.NewTimer(outputSafetyTimeout)
	defer timer.Stop()
	var completed executionResult
	select {
	case completed = <-finished:
		close(release)
	case <-timer.C:
		close(release)
		completed = <-finished
		t.Fatalf("capture exceeded its cleanup deadline: (%#v, %v)", completed.result, completed.err)
	}
	result, runErr := completed.result, completed.err
	if !errors.Is(runErr, ErrCleanup) || result.Outcome != OutcomeCleanupFailure || result.Stdout != nil || result.Stderr != nil {
		t.Fatalf("executeOwnedWithCapture = (%#v, %v)", result, runErr)
	}
	if closeErr := <-heldClosed; closeErr != nil {
		t.Fatal(closeErr)
	}
	assertCaptureGoroutineExited(t, createdCapture.stdout.done)
	assertCaptureGoroutineExited(t, createdCapture.stderr.done)
}

func assertCaptureGoroutineExited(t *testing.T, done <-chan error) {
	t.Helper()
	timer := time.NewTimer(outputSafetyTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatal("capture goroutine did not exit")
	}
}

type executionResult struct {
	result Result
	err    error
}

func awaitUnjoinedExecution(
	t *testing.T,
	owner *unjoinedOwner,
	finished <-chan executionResult,
	waitDone <-chan struct{},
) executionResult {
	t.Helper()
	startDeadline := time.NewTimer(outputSafetyTimeout)
	defer startDeadline.Stop()
	select {
	case <-owner.started:
		startDeadline.Stop()
	case <-startDeadline.C:
		t.Fatal("process did not start")
	}
	completionDeadline := time.NewTimer(outputSafetyTimeout)
	defer completionDeadline.Stop()
	select {
	case got := <-finished:
		return got
	case <-completionDeadline.C:
		terminateUnjoinedFixture(t, owner, waitDone)
		t.Fatal("execute did not respect the cleanup deadline")
	}
	return executionResult{}
}

func terminateUnjoinedFixture(t *testing.T, owner *unjoinedOwner, waitDone <-chan struct{}) {
	t.Helper()
	err := owner.command.Process.Kill()
	<-waitDone
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("fallback termination failed: %v", err)
	}
}

func TestExecuteRejectsCaptureFailure(t *testing.T) {
	t.Parallel()
	command := validCommand(t)
	failure := errors.New("capture")
	tests := []struct {
		name    string
		failure error
		outcome Outcome
	}{
		{name: "start", failure: failure, outcome: OutcomeStartFailure},
		{name: "cleanup", failure: errors.Join(failure, ErrCleanup), outcome: OutcomeCleanupFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			create := func(int, int, chan struct{}, *sync.Once) (*outputCapture, error) { return nil, test.failure }
			result, err := executeOwnedWithCapture(t.Context(), command, ownerWith(&blockedOwner{}), nil, create)
			if !errors.Is(err, ErrStart) || !errors.Is(err, failure) || result.Outcome != test.outcome || result.Started {
				t.Fatalf("executeOwnedWithCapture = (%#v, %v)", result, err)
			}
		})
	}
}

func TestRelativeContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := relativeContext(t.Context(), 0)
	cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("cancelled context error = %v", ctx.Err())
	}
	ctx, cancel = relativeContext(t.Context(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("deadline context error = %v", ctx.Err())
	}
}

func TestRunHelpers(t *testing.T) {
	t.Parallel()
	if outcome, err := waitOutcome(errors.New("wait"), nil); outcome != OutcomeCleanupFailure || !errors.Is(err, ErrCleanup) {
		t.Fatalf("waitOutcome = (%s, %v)", outcome, err)
	}
	if code := exitCode(&exec.Cmd{}); code != -1 {
		t.Fatalf("exitCode = %d", code)
	}
	overflow := make(chan struct{})
	once := &sync.Once{}
	buffer := boundedBuffer{limit: 1, overflow: overflow, once: once}
	if _, err := buffer.Write([]byte("excess")); !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("overflow error = %v", err)
	}
	if written, err := buffer.Write([]byte("later")); written != 0 || !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("post-overflow = (%d, %v)", written, err)
	}
	waited := make(chan error, 1)
	waited <- errors.New("wait")
	if waitErr, cleanupErr, joined := waitAfterCleanupDeadline(waited); waitErr == nil || cleanupErr != nil || !joined {
		t.Fatalf("deadline recheck = (%v, %v, %t)", waitErr, cleanupErr, joined)
	}
}

type fakeOwner struct {
	passiveOwner
	command        *exec.Cmd
	afterStart     func()
	startErr       error
	terminateErr   error
	waitErr        error
	closeErr       error
	skipFirstKill  bool
	terminateCalls int
}

func (owner *fakeOwner) start(command *exec.Cmd, _ Command) error {
	if owner.startErr != nil {
		return owner.startErr
	}
	if err := command.Start(); err != nil {
		return errors.Join(ErrStart, err)
	}
	owner.command = command
	if owner.afterStart != nil {
		owner.afterStart()
	}
	return nil
}

func (owner *fakeOwner) terminate() error {
	owner.terminateCalls++
	if owner.command != nil && owner.command.Process != nil && (!owner.skipFirstKill || owner.terminateCalls != 1) {
		return errors.Join(owner.command.Process.Kill(), owner.terminateErr)
	}
	return owner.terminateErr
}

func (owner *fakeOwner) wait(time.Time) error  { return owner.waitErr }
func (owner *fakeOwner) close(time.Time) error { return owner.closeErr }

func ownerWith(owner processOwner) ownerFactory {
	return func() (processOwner, error) { return owner, nil }
}

type blockedOwner struct {
	passiveOwner
	afterStart func()
}

func (owner *blockedOwner) start(*exec.Cmd, Command) error {
	owner.afterStart()
	return nil
}

func (*blockedOwner) terminate() error      { return errors.New("terminate") }
func (*blockedOwner) wait(time.Time) error  { return errors.New("wait") }
func (*blockedOwner) close(time.Time) error { return errors.New("close") }

type passiveOwner struct{}

func (*passiveOwner) start(*exec.Cmd, Command) error { return nil }
func (*passiveOwner) terminate() error               { return nil }
func (*passiveOwner) wait(time.Time) error           { return nil }
func (*passiveOwner) close(time.Time) error          { return nil }
func (*passiveOwner) health() <-chan struct{}        { return nil }
func (*passiveOwner) healthError() error             { return nil }

type cancellingOwner struct {
	passiveOwner
	cancel context.CancelFunc
}

func (owner *cancellingOwner) start(*exec.Cmd, Command) error { owner.cancel(); return nil }

type failedHealthOwner struct {
	passiveOwner
	done <-chan struct{}
	err  error
}

func (owner *failedHealthOwner) health() <-chan struct{} { return owner.done }
func (owner *failedHealthOwner) healthError() error      { return owner.err }

type stagedHealth struct {
	done    chan struct{}
	pending chan struct{}
	calls   int
	readyAt int
}

func newStagedHealth(readyAt int) stagedHealth {
	done := make(chan struct{})
	close(done)
	return stagedHealth{done: done, pending: make(chan struct{}), readyAt: readyAt}
}

func (health *stagedHealth) channel() <-chan struct{} {
	health.calls++
	if health.calls < health.readyAt {
		return health.pending
	}
	return health.done
}

type stagedHealthOwner struct {
	passiveOwner
	staged stagedHealth
	err    error
}

type healthCancellationOwner struct {
	passiveOwner
	staged     stagedHealth
	released   chan struct{}
	release    sync.Once
	healthSeen bool
}

func newHealthCancellationOwner(readyAt int) *healthCancellationOwner {
	return &healthCancellationOwner{
		staged: newStagedHealth(readyAt), released: make(chan struct{}),
	}
}

func (owner *healthCancellationOwner) terminate() error {
	owner.release.Do(func() { close(owner.released) })
	return nil
}
func (owner *healthCancellationOwner) close(time.Time) error {
	if !owner.healthSeen {
		return ErrOwnership
	}
	return nil
}
func (owner *healthCancellationOwner) health() <-chan struct{} { return owner.staged.channel() }
func (owner *healthCancellationOwner) healthError() error {
	owner.healthSeen = true
	return ErrOwnership
}

func newStagedHealthOwner(err error, readyAt int) *stagedHealthOwner {
	return &stagedHealthOwner{staged: newStagedHealth(readyAt), err: err}
}

func (owner *stagedHealthOwner) health() <-chan struct{} { return owner.staged.channel() }
func (owner *stagedHealthOwner) healthError() error      { return owner.err }

type deadlineOwner struct {
	passiveOwner
	waitDeadline  time.Time
	closeDeadline time.Time
}

func (owner *deadlineOwner) wait(deadline time.Time) error {
	owner.waitDeadline = deadline
	return nil
}
func (owner *deadlineOwner) close(deadline time.Time) error {
	owner.closeDeadline = deadline
	return nil
}

type unjoinedOwner struct {
	passiveOwner
	command    *exec.Cmd
	afterStart func()
	started    chan struct{}
}

func (owner *unjoinedOwner) start(command *exec.Cmd, _ Command) error {
	if err := command.Start(); err != nil {
		return err
	}
	owner.command = command
	close(owner.started)
	owner.afterStart()
	return nil
}
