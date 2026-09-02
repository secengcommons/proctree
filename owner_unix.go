//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package proctree

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type unixAPI struct {
	pipe       func() (*os.File, *os.File, error)
	executable func() (string, error)
	start      func(*exec.Cmd) error
	close      func(*os.File) error
	kill       func(int, syscall.Signal) error
	now        func() time.Time
	sleep      func(time.Duration)
	timer      func(time.Duration) ownerTimer
	ready      func(*os.File, time.Time) error
}

type ownerTimer interface {
	channel() <-chan time.Time
	stop() bool
}

type systemTimer struct{ timer *time.Timer }

func (timer systemTimer) channel() <-chan time.Time { return timer.timer.C }
func (timer systemTimer) stop() bool                { return timer.timer.Stop() }

func systemUnixAPI() unixAPI {
	return unixAPI{
		pipe: os.Pipe, executable: os.Executable, start: func(command *exec.Cmd) error { return command.Start() },
		close: func(file *os.File) error { return file.Close() }, kill: syscall.Kill,
		now: time.Now, sleep: time.Sleep, timer: func(duration time.Duration) ownerTimer { return systemTimer{timer: time.NewTimer(duration)} },
		ready: readWatchdogReady,
	}
}

type unixOwner struct {
	pid          int
	control      *os.File
	watchdogDone chan struct{}
	watchdogErr  error
	watchdogMu   sync.Mutex
	watchdogStop bool
	watchdogLost bool
	healthSeen   bool
	api          unixAPI
	terminateMu  sync.Mutex
	terminated   bool
	stopOnce     sync.Once
	stopErr      error
}

func newProcessOwner() (processOwner, error) {
	return &unixOwner{api: systemUnixAPI()}, nil
}

func (owner *unixOwner) start(command *exec.Cmd, admitted Command) error {
	controlRead, controlWrite, err := owner.api.pipe()
	if err != nil {
		return errors.Join(ErrOwnership, err)
	}
	watchdog, err := owner.watchdogCommand()
	if err != nil {
		cleanupErr := errors.Join(owner.api.close(controlRead), owner.api.close(controlWrite))
		return errors.Join(ErrOwnership, err, cleanupError(cleanupErr))
	}
	readyRead, readyWrite, err := owner.api.pipe()
	if err != nil {
		cleanupErr := errors.Join(owner.api.close(controlRead), owner.api.close(controlWrite))
		return errors.Join(ErrOwnership, err, cleanupError(cleanupErr))
	}
	watchdog.ExtraFiles = append(watchdog.ExtraFiles, controlRead, readyWrite)
	watchdog.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = owner.api.start(watchdog); err != nil {
		cleanupErr := errors.Join(
			owner.api.close(controlRead), owner.api.close(controlWrite), owner.api.close(readyRead), owner.api.close(readyWrite),
		)
		return errors.Join(ErrOwnership, err, cleanupError(cleanupErr))
	}
	owner.pid = watchdog.Process.Pid
	owner.control = controlWrite
	owner.watchdogDone = make(chan struct{})
	go func() {
		owner.recordWatchdogExit(watchdog.Wait())
	}()
	if err = errors.Join(owner.api.close(controlRead), owner.api.close(readyWrite)); err != nil {
		deadline := owner.api.now().Add(admitted.CleanupTimeout)
		cleanupErr := errors.Join(owner.api.close(readyRead), owner.close(deadline))
		return withCleanupDeadline(errors.Join(ErrOwnership, err, cleanupError(cleanupErr)), deadline)
	}
	deadline := owner.api.now().Add(admitted.CleanupTimeout)
	if err = owner.api.ready(readyRead, deadline); err != nil {
		return withCleanupDeadline(errors.Join(ErrOwnership, err, cleanupError(owner.close(deadline))), deadline)
	}
	select {
	case <-owner.watchdogDone:
		healthErr := owner.healthError()
		return withCleanupDeadline(errors.Join(healthErr, cleanupError(owner.close(deadline))), deadline)
	default:
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: owner.pid}
	if err = owner.api.start(command); err != nil {
		deadline := owner.api.now().Add(admitted.CleanupTimeout)
		select {
		case <-owner.watchdogDone:
			healthErr := owner.healthError()
			return withCleanupDeadline(errors.Join(healthErr, err, cleanupError(owner.close(deadline))), deadline)
		default:
		}
		return withCleanupDeadline(errors.Join(ErrStart, err, cleanupError(owner.close(deadline))), deadline)
	}
	return nil
}

func readWatchdogReady(file *os.File, deadline time.Time) error {
	if err := file.SetReadDeadline(deadline); err != nil {
		return errors.Join(err, file.Close())
	}
	var signal [1]byte
	_, readErr := io.ReadFull(file, signal[:])
	if readErr != nil {
		return errors.Join(readErr, file.Close())
	}
	if signal[0] != watchdogReadySignal {
		return errors.Join(errors.New("process watchdog returned an invalid readiness signal"), file.Close())
	}
	var trailing [1]byte
	count, terminalErr := file.Read(trailing[:])
	closeErr := file.Close()
	if count != 0 {
		return errors.Join(errors.New("process watchdog returned trailing readiness data"), terminalErr, closeErr)
	}
	if !errors.Is(terminalErr, io.EOF) {
		return errors.Join(errors.New("process watchdog did not terminate its readiness frame"), terminalErr, closeErr)
	}
	return closeErr
}

func (owner *unixOwner) recordWatchdogExit(err error) {
	owner.watchdogMu.Lock()
	owner.watchdogErr = err
	owner.watchdogLost = !owner.watchdogStop
	close(owner.watchdogDone)
	owner.watchdogMu.Unlock()
}

func (owner *unixOwner) beginWatchdogStop() {
	owner.watchdogMu.Lock()
	owner.watchdogStop = true
	owner.watchdogMu.Unlock()
}

func (owner *unixOwner) health() <-chan struct{} { return owner.watchdogDone }

func (owner *unixOwner) healthError() error {
	owner.watchdogMu.Lock()
	defer owner.watchdogMu.Unlock()
	owner.healthSeen = true
	return unexpectedWatchdogError(owner.watchdogErr)
}

func (owner *unixOwner) watchdogCommand() (*exec.Cmd, error) {
	executable, err := owner.api.executable()
	if err != nil {
		return nil, err
	}
	return &exec.Cmd{Path: executable, Args: []string{executable, watchdogArgument}, Env: []string{}}, nil
}

func (owner *unixOwner) terminate() error {
	if owner.pid <= 0 {
		return nil
	}
	owner.terminateMu.Lock()
	defer owner.terminateMu.Unlock()
	if owner.terminated {
		return nil
	}
	owner.beginWatchdogStop()
	err := owner.api.kill(-owner.pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		err = nil
	}
	if err == nil {
		owner.terminated = true
	}
	return err
}

func (owner *unixOwner) wait(deadline time.Time) error {
	stopErr := owner.stopWatchdogUntil(deadline)
	for {
		err := owner.api.kill(-owner.pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return stopErr
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return errors.Join(stopErr, err)
		}
		if !owner.api.now().Before(deadline) {
			return errors.Join(stopErr, err, errors.New("process group did not become empty"))
		}
		owner.api.sleep(time.Millisecond)
	}
}

func (owner *unixOwner) close(deadline time.Time) error { return owner.stopWatchdogUntil(deadline) }

func (owner *unixOwner) stopWatchdogUntil(deadline time.Time) error {
	owner.stopOnce.Do(func() {
		owner.beginWatchdogStop()
		if owner.control != nil {
			owner.stopErr = owner.api.close(owner.control)
		}
		if owner.watchdogDone == nil {
			return
		}
		timer := owner.api.timer(owner.remaining(deadline))
		defer stopTimer(timer)
		select {
		case <-owner.watchdogDone:
			owner.stopErr = errors.Join(owner.stopErr, owner.watchdogCleanupError())
		case <-timer.channel():
			killErr := owner.terminate()
			second := owner.api.timer(owner.remaining(deadline))
			defer stopTimer(second)
			select {
			case <-owner.watchdogDone:
				owner.stopErr = errors.Join(owner.stopErr, killErr, owner.watchdogCleanupError())
			case <-second.channel():
				owner.stopErr = errors.Join(owner.stopErr, owner.watchdogAfterDeadline(killErr))
			}
		}
	})
	return owner.stopErr
}

func (owner *unixOwner) watchdogAfterDeadline(killErr error) error {
	select {
	case <-owner.watchdogDone:
		return errors.Join(killErr, owner.watchdogCleanupError())
	default:
		return errors.Join(killErr, errors.New("process watchdog did not terminate"))
	}
}

func (owner *unixOwner) watchdogCleanupError() error {
	owner.watchdogMu.Lock()
	defer owner.watchdogMu.Unlock()
	if owner.watchdogLost {
		if owner.healthSeen {
			return nil
		}
		return unexpectedWatchdogError(owner.watchdogErr)
	}
	return watchdogWaitError(owner.watchdogErr)
}

func unexpectedWatchdogError(err error) error {
	failure := errors.New("process watchdog exited unexpectedly")
	return errors.Join(ErrOwnership, failure, err)
}

func (owner *unixOwner) remaining(deadline time.Time) time.Duration {
	return max(0, deadline.Sub(owner.api.now()))
}

func stopTimer(timer ownerTimer) {
	if !timer.stop() {
		select {
		case <-timer.channel():
		default:
		}
	}
}

func watchdogWaitError(err error) error {
	if err == nil {
		return errors.New("process watchdog exited without terminating its group")
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, statusOK := exitErr.Sys().(syscall.WaitStatus); statusOK && status.Signaled() && status.Signal() == syscall.SIGKILL {
			return nil
		}
	}
	return err
}
