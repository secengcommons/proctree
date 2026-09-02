package proctree

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
)

type processOwner interface {
	start(*exec.Cmd, Command) error
	terminate() error
	wait(time.Time) error
	close(time.Time) error
	health() <-chan struct{}
	healthError() error
}

type ownerFactory func() (processOwner, error)

func Run(ctx context.Context, command Command) (Result, error) {
	return run(ctx, command, newProcessOwner)
}

func run(ctx context.Context, command Command, factory ownerFactory) (Result, error) {
	if ctx == nil {
		return Result{ExitCode: -1, Outcome: OutcomeInvalid}, ErrInvalid
	}
	admitted, err := admit(command)
	if err != nil {
		return Result{ExitCode: -1, Outcome: OutcomeInvalid}, err
	}
	ctx, cancel := relativeContext(ctx, admitted.Timeout)
	defer cancel()
	if err = ctx.Err(); err != nil {
		outcome, resultErr := contextOutcome(err)
		return Result{ExitCode: -1, Outcome: outcome}, resultErr
	}
	return executeOwned(ctx, admitted, factory)
}

func executeOwned(ctx context.Context, admitted Command, factory ownerFactory) (Result, error) {
	return executeOwnedWithWait(ctx, admitted, factory, func(command *exec.Cmd) error { return command.Wait() })
}

func executeOwnedWithWait(
	ctx context.Context,
	admitted Command,
	factory ownerFactory,
	waitCommand func(*exec.Cmd) error,
) (Result, error) {
	return executeOwnedWithCapture(ctx, admitted, factory, waitCommand, newOutputCapture)
}

func executeOwnedWithCapture(
	ctx context.Context,
	admitted Command,
	factory ownerFactory,
	waitCommand func(*exec.Cmd) error,
	createCapture func(int, int, chan struct{}, *sync.Once) (*outputCapture, error),
) (Result, error) {
	overflow := make(chan struct{})
	var overflowOnce sync.Once
	capture, err := createCapture(admitted.StdoutLimit, admitted.StderrLimit, overflow, &overflowOnce)
	if err != nil {
		return captureStartFailure(err)
	}
	capture.start()
	commandLine := &exec.Cmd{
		Path: admitted.Executable, Args: append([]string{admitted.Executable}, admitted.Arguments...),
		Dir: admitted.Directory, Env: admitted.Environment, Stdin: bytes.NewReader(admitted.Input),
		Stdout: capture.stdout.write, Stderr: capture.stderr.write,
	}
	owner, earlyResult, earlyErr, finished := prepareOwner(ctx, admitted, factory, capture)
	if finished {
		return earlyResult, earlyErr
	}
	if err = owner.start(commandLine, admitted); err != nil {
		return finishStartFailure(owner, capture, admitted.CleanupTimeout, err)
	}
	streamSetupErr := capture.closeWriters()
	waited := make(chan error, 1)
	go func() { waited <- waitCommand(commandLine) }()
	waitResult := await(ctx, overflow, owner, waited)
	deadline := waitResult.cleanupStarted.Add(admitted.CleanupTimeout)
	cleanupErr := errors.Join(streamSetupErr, waitResult.cleanupErr, cleanupOwner(owner, deadline))
	if !waitResult.joined {
		var lateCleanupErr error
		waitResult.waitErr, lateCleanupErr, waitResult.joined = waitForCleanup(waited, deadline)
		cleanupErr = errors.Join(cleanupErr, lateCleanupErr)
	}
	if !waitResult.joined {
		cleanupErr = errors.Join(cleanupErr, capture.closeReaders())
	}
	captureJoined, captureErr := capture.finish(deadline)
	cleanupErr = errors.Join(cleanupErr, captureErr)
	if !waitResult.joined || !captureJoined {
		return Result{Started: true, ExitCode: -1, Outcome: OutcomeCleanupFailure}, errors.Join(waitResult.primaryErr, ErrCleanup, cleanupErr)
	}
	if capture.stdout.buffer.full || capture.stderr.buffer.full {
		applyOutputLimit(&waitResult)
	} else if waitResult.primaryErr == nil {
		waitResult.outcome, waitResult.primaryErr = waitOutcome(waitResult.waitErr, commandLine.ProcessState)
	}
	result := Result{
		Started: true, ExitCode: exitCode(commandLine),
		Stdout: capture.stdout.buffer.bytes(), Stderr: capture.stderr.buffer.bytes(), Outcome: waitResult.outcome,
	}
	if cleanupErr != nil {
		result.Outcome = OutcomeCleanupFailure
		return result, errors.Join(waitResult.primaryErr, ErrCleanup, cleanupErr, capture.stdout.buffer.err(), capture.stderr.buffer.err())
	}
	return result, errors.Join(waitResult.primaryErr, capture.stdout.buffer.err(), capture.stderr.buffer.err())
}

func captureStartFailure(err error) (Result, error) {
	outcome := OutcomeStartFailure
	if errors.Is(err, ErrCleanup) {
		outcome = OutcomeCleanupFailure
	}
	return Result{ExitCode: -1, Outcome: outcome}, errors.Join(ErrStart, err)
}

func finishStartFailure(owner processOwner, capture *outputCapture, timeout time.Duration, startErr error) (Result, error) {
	deadline := startCleanupDeadline(startErr, timeout)
	ownerErr := owner.close(deadline)
	_, captureErr := capture.finish(deadline)
	closeErr := errors.Join(ownerErr, captureErr)
	result := Result{ExitCode: -1, Outcome: ownerFailureOutcome(startErr)}
	if errors.Is(startErr, ErrCleanup) || closeErr != nil {
		result.Outcome = OutcomeCleanupFailure
	}
	return result, errors.Join(startErr, cleanupError(closeErr))
}

func prepareOwner(
	ctx context.Context,
	admitted Command,
	factory ownerFactory,
	capture *outputCapture,
) (processOwner, Result, error, bool) {
	owner, err := factory()
	if err != nil {
		result := Result{ExitCode: -1, Outcome: ownerFailureOutcome(err)}
		_, captureErr := capture.finish(time.Now().Add(admitted.CleanupTimeout))
		if errors.Is(err, ErrCleanup) || captureErr != nil {
			result.Outcome = OutcomeCleanupFailure
		}
		return nil, result, errors.Join(err, cleanupError(captureErr)), true
	}
	if err = ctx.Err(); err == nil {
		return owner, Result{}, nil, false
	}
	outcome, primaryErr := contextOutcome(err)
	deadline := time.Now().Add(admitted.CleanupTimeout)
	ownerErr := owner.close(deadline)
	_, captureErr := capture.finish(deadline)
	closeErr := errors.Join(ownerErr, captureErr)
	if closeErr != nil {
		outcome = OutcomeCleanupFailure
	}
	result := Result{ExitCode: -1, Outcome: outcome}
	return nil, result, errors.Join(primaryErr, cleanupError(closeErr)), true
}

func relativeContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout == 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

type executionWait struct {
	waitErr        error
	outcome        Outcome
	primaryErr     error
	cleanupErr     error
	joined         bool
	cleanupStarted time.Time
}

func await(ctx context.Context, overflow <-chan struct{}, owner processOwner, waited <-chan error) executionWait {
	if err := ctx.Err(); err != nil {
		return cancelExecution(owner, err)
	}
	if healthErr, failed := ownerHealthFailure(owner); failed {
		if contextErr := ctx.Err(); contextErr != nil {
			return cancelExecutionWithHealth(owner, contextErr, healthErr)
		}
		return ownerHealthExecution(owner, healthErr, nil, false)
	}
	select {
	case err := <-waited:
		return completedExecution(ctx, owner, err)
	case <-overflow:
		return overflowExecution(ctx, owner)
	case <-ctx.Done():
		return cancelExecution(owner, ctx.Err())
	case <-owner.health():
		return selectedHealthExecution(ctx, owner)
	}
}

func completedExecution(ctx context.Context, owner processOwner, waitErr error) executionWait {
	if contextErr := ctx.Err(); contextErr != nil {
		result := cancelExecution(owner, contextErr)
		result.waitErr, result.joined = waitErr, true
		return result
	}
	if healthErr, failed := ownerHealthFailure(owner); failed {
		return ownerHealthExecution(owner, healthErr, waitErr, true)
	}
	return executionWait{waitErr: waitErr, joined: true, cleanupStarted: time.Now()}
}

func overflowExecution(ctx context.Context, owner processOwner) executionWait {
	if contextErr := ctx.Err(); contextErr != nil {
		return cancelExecution(owner, contextErr)
	}
	if healthErr, failed := ownerHealthFailure(owner); failed {
		return ownerHealthExecution(owner, healthErr, nil, false)
	}
	started := time.Now()
	terminateErr := owner.terminate()
	return executionWait{outcome: OutcomeOutputLimit, primaryErr: ErrOutputLimit, cleanupErr: terminateErr, cleanupStarted: started}
}

func selectedHealthExecution(ctx context.Context, owner processOwner) executionWait {
	healthErr := owner.healthError()
	if contextErr := ctx.Err(); contextErr != nil {
		return cancelExecutionWithHealth(owner, contextErr, healthErr)
	}
	return ownerHealthExecution(owner, healthErr, nil, false)
}

func ownerHealthFailure(owner processOwner) (error, bool) {
	select {
	case <-owner.health():
		return owner.healthError(), true
	default:
		return nil, false
	}
}

func ownerHealthExecution(owner processOwner, healthErr, waitErr error, joined bool) executionWait {
	started := time.Now()
	terminateErr := owner.terminate()
	return executionWait{
		waitErr: waitErr, outcome: OutcomeOwnershipFailure, primaryErr: healthErr,
		cleanupErr: terminateErr, joined: joined, cleanupStarted: started,
	}
}

func cancelExecution(owner processOwner, contextErr error) executionWait {
	healthErr, _ := ownerHealthFailure(owner)
	return cancelExecutionWithHealth(owner, contextErr, healthErr)
}

func cancelExecutionWithHealth(owner processOwner, contextErr, healthErr error) executionWait {
	started := time.Now()
	outcome, err := contextOutcome(contextErr)
	terminateErr := owner.terminate()
	return executionWait{outcome: outcome, primaryErr: errors.Join(err, healthErr), cleanupErr: terminateErr, cleanupStarted: started}
}

func cleanupOwner(owner processOwner, deadline time.Time) error {
	return errors.Join(owner.terminate(), owner.wait(deadline), owner.close(deadline))
}

func remainingCleanup(deadline time.Time) time.Duration { return max(0, time.Until(deadline)) }

func applyOutputLimit(result *executionWait) {
	if result.outcome == "" {
		result.outcome = OutcomeOutputLimit
	}
	result.primaryErr = errors.Join(result.primaryErr, ErrOutputLimit)
}

type boundedStartError struct {
	cause    error
	deadline time.Time
}

func (err *boundedStartError) Error() string { return err.cause.Error() }
func (err *boundedStartError) Unwrap() error { return err.cause }

func withCleanupDeadline(cause error, deadline time.Time) error {
	return &boundedStartError{cause: cause, deadline: deadline}
}

func startCleanupDeadline(err error, timeout time.Duration) time.Time {
	var bounded *boundedStartError
	if errors.As(err, &bounded) {
		return bounded.deadline
	}
	return time.Now().Add(timeout)
}

func waitForCleanup(waited <-chan error, deadline time.Time) (error, error, bool) {
	timer := time.NewTimer(remainingCleanup(deadline))
	defer timer.Stop()
	select {
	case err := <-waited:
		return err, nil, true
	case <-timer.C:
		return waitAfterCleanupDeadline(waited)
	}
}

func waitAfterCleanupDeadline(waited <-chan error) (error, error, bool) {
	select {
	case err := <-waited:
		return err, nil, true
	default:
		return nil, errors.Join(ErrCleanup, errors.New("process did not terminate before cleanup deadline")), false
	}
}

func contextOutcome(err error) (Outcome, error) {
	if errors.Is(err, context.DeadlineExceeded) {
		return OutcomeDeadline, errors.Join(ErrDeadline, err)
	}
	return OutcomeCancelled, errors.Join(ErrCancelled, err)
}

func waitOutcome(err error, state *os.ProcessState) (Outcome, error) {
	if err == nil && state != nil && state.Success() {
		return OutcomeCompleted, nil
	}
	if state != nil && !state.Success() {
		return OutcomeExitFailure, errors.Join(ErrExit, err)
	}
	return OutcomeCleanupFailure, errors.Join(ErrCleanup, err)
}

func ownerFailureOutcome(err error) Outcome {
	switch {
	case errors.Is(err, ErrUnsupported):
		return OutcomeUnsupported
	case errors.Is(err, ErrStart):
		return OutcomeStartFailure
	default:
		return OutcomeOwnershipFailure
	}
}

func cleanupError(err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(ErrCleanup, err)
}

func exitCode(command *exec.Cmd) int {
	if command.ProcessState == nil {
		return -1
	}
	return command.ProcessState.ExitCode()
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	full     bool
	overflow chan struct{}
	once     *sync.Once
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	if buffer.full {
		return 0, ErrOutputLimit
	}
	remaining := buffer.limit - buffer.buffer.Len()
	if len(value) > remaining {
		if remaining > 0 {
			_, _ = buffer.buffer.Write(value[:remaining])
		}
		buffer.full = true
		buffer.once.Do(func() { close(buffer.overflow) })
		return remaining, ErrOutputLimit
	}
	return buffer.buffer.Write(value)
}

func (buffer *boundedBuffer) bytes() []byte { return buffer.buffer.Bytes() }

func (buffer *boundedBuffer) err() error {
	if buffer.full {
		return ErrOutputLimit
	}
	return nil
}
