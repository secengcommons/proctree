//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package proctree

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestUnixOwnerStartFailures(t *testing.T) {
	t.Run("pipe", func(t *testing.T) {
		api := systemUnixAPI()
		failure := errors.New("pipe")
		api.pipe = func() (*os.File, *os.File, error) { return nil, nil, failure }
		if err := (&unixOwner{api: api}).start(&exec.Cmd{}, cleanupCommand()); !errors.Is(err, ErrOwnership) || !errors.Is(err, failure) {
			t.Fatalf("pipe error = %v", err)
		}
	})
	t.Run("executable", func(t *testing.T) {
		api := systemUnixAPI()
		failure := errors.New("executable")
		closeFailure := errors.New("close")
		api.executable = func() (string, error) { return "", failure }
		api.close = func(file *os.File) error { return errors.Join(file.Close(), closeFailure) }
		if err := (&unixOwner{api: api}).start(&exec.Cmd{}, cleanupCommand()); !errors.Is(err, failure) || !errors.Is(err, closeFailure) || !errors.Is(err, ErrCleanup) {
			t.Fatalf("executable error = %v", err)
		}
	})
	t.Run("watchdog start", func(t *testing.T) {
		api := systemUnixAPI()
		failure := errors.New("start")
		api.start = func(*exec.Cmd) error { return failure }
		if err := (&unixOwner{api: api}).start(&exec.Cmd{}, cleanupCommand()); !errors.Is(err, ErrOwnership) || !errors.Is(err, failure) {
			t.Fatalf("watchdog start error = %v", err)
		}
	})
}

func TestUnixOwnerReadinessFailures(t *testing.T) {
	t.Run("readiness pipe", func(t *testing.T) {
		api := systemUnixAPI()
		failure := errors.New("ready pipe")
		calls := 0
		api.pipe = func() (*os.File, *os.File, error) {
			calls++
			if calls == 2 {
				return nil, nil, failure
			}
			return os.Pipe()
		}
		if err := (&unixOwner{api: api}).start(&exec.Cmd{}, cleanupCommand()); !errors.Is(err, ErrOwnership) || !errors.Is(err, failure) {
			t.Fatalf("readiness pipe error = %v", err)
		}
	})
	t.Run("watchdog readiness", func(t *testing.T) {
		api := systemUnixAPI()
		starts := 0
		targetStarted := false
		api.start = func(command *exec.Cmd) error {
			starts++
			if starts == 1 {
				command.Path = "/bin/sh"
				command.Args = []string{"sh", "-c", "exit 0"}
				return command.Start()
			}
			targetStarted = true
			return nil
		}
		err := (&unixOwner{api: api}).start(&exec.Cmd{}, cleanupCommand())
		if !errors.Is(err, ErrOwnership) || targetStarted {
			t.Fatalf("watchdog readiness = (%t, %v)", targetStarted, err)
		}
	})
}

func TestUnixOwnerHealthFailures(t *testing.T) {
	t.Run("watchdog health", func(t *testing.T) {
		api := systemUnixAPI()
		owner := &unixOwner{}
		starts := 0
		targetStarted := false
		api.start = func(command *exec.Cmd) error {
			starts++
			if starts == 2 {
				targetStarted = true
			}
			return command.Start()
		}
		api.ready = func(file *os.File, deadline time.Time) error {
			if err := readWatchdogReady(file, deadline); err != nil {
				return err
			}
			if err := syscall.Kill(owner.pid, syscall.SIGKILL); err != nil {
				return err
			}
			<-owner.watchdogDone
			return nil
		}
		owner.api = api
		err := owner.start(&exec.Cmd{}, cleanupCommand())
		if !errors.Is(err, ErrOwnership) || targetStarted {
			t.Fatalf("watchdog health = (%t, %v)", targetStarted, err)
		}
	})
	t.Run("watchdog loss during target start", func(t *testing.T) {
		api := systemUnixAPI()
		owner := &unixOwner{}
		failure := errors.New("target")
		starts := 0
		api.start = func(command *exec.Cmd) error {
			starts++
			if starts == 1 {
				return command.Start()
			}
			if err := syscall.Kill(owner.pid, syscall.SIGKILL); err != nil {
				return err
			}
			<-owner.watchdogDone
			return failure
		}
		owner.api = api
		err := owner.start(&exec.Cmd{}, cleanupCommand())
		if !errors.Is(err, ErrOwnership) || !errors.Is(err, failure) || errors.Is(err, ErrCleanup) {
			t.Fatalf("target start ownership error = %v", err)
		}
	})
}

func TestUnixOwnerTargetStartFailures(t *testing.T) {
	t.Run("target start", func(t *testing.T) {
		api := systemUnixAPI()
		failure := errors.New("target")
		starts := 0
		api.start = func(command *exec.Cmd) error {
			starts++
			if starts == 2 {
				return failure
			}
			return command.Start()
		}
		err := (&unixOwner{api: api}).start(&exec.Cmd{}, cleanupCommand())
		var bounded *boundedStartError
		if !errors.Is(err, ErrStart) || !errors.Is(err, failure) || !errors.As(err, &bounded) {
			t.Fatalf("target start error = %v", err)
		}
	})
	t.Run("control close", func(t *testing.T) {
		api := systemUnixAPI()
		failure := errors.New("close")
		calls := 0
		api.close = func(file *os.File) error {
			calls++
			err := file.Close()
			if calls == 1 {
				return errors.Join(err, failure)
			}
			return err
		}
		err := (&unixOwner{api: api}).start(&exec.Cmd{}, cleanupCommand())
		var bounded *boundedStartError
		if !errors.Is(err, ErrOwnership) || !errors.Is(err, failure) || !errors.As(err, &bounded) {
			t.Fatalf("control close error = %v", err)
		}
	})
}

func TestReadWatchdogReadyRejectsClosedDescriptor(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "closed-")
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = readWatchdogReady(file, time.Now()); err == nil {
		t.Fatal("closed readiness descriptor accepted")
	}
}

func TestReadWatchdogReadySignals(t *testing.T) {
	for name, signal := range map[string]byte{"invalid": 0, "valid": watchdogReadySignal} {
		t.Run(name, func(t *testing.T) {
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			if _, err = write.Write([]byte{signal}); err != nil {
				t.Fatal(err)
			}
			if err = write.Close(); err != nil {
				t.Fatal(err)
			}
			err = readWatchdogReady(read, time.Now().Add(DefaultCleanupTimeout))
			if (name == "valid" && err != nil) || (name == "invalid" && err == nil) {
				t.Fatalf("readiness result = %v", err)
			}
		})
	}
}

func TestReadWatchdogReadyRejectsTrailingData(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = write.Write([]byte{watchdogReadySignal, 'X'}); err != nil {
		t.Fatal(err)
	}
	if err = write.Close(); err != nil {
		t.Fatal(err)
	}
	if err = readWatchdogReady(read, time.Now().Add(DefaultCleanupTimeout)); err == nil {
		t.Fatal("trailing readiness data accepted")
	}
}

func TestReadWatchdogReadyRequiresClosedFrame(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := write.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if _, err = write.Write([]byte{watchdogReadySignal}); err != nil {
		t.Fatal(err)
	}
	if err = readWatchdogReady(read, time.Now().Add(failedTerminationDeadline)); err == nil {
		t.Fatal("unclosed readiness frame accepted")
	}
}

func TestUnixOwnerWatchdogCommand(t *testing.T) {
	api := systemUnixAPI()
	failure := errors.New("executable")
	api.executable = func() (string, error) { return "", failure }
	owner := &unixOwner{api: api}
	if _, err := owner.watchdogCommand(); !errors.Is(err, failure) {
		t.Fatalf("watchdog command error = %v", err)
	}
	api.executable = func() (string, error) { return "/fixture/proctree", nil }
	owner.api = api
	command, err := owner.watchdogCommand()
	if err != nil || command.Path != "/fixture/proctree" || len(command.Args) != 2 || command.Args[1] != watchdogArgument || command.Env == nil {
		t.Fatalf("watchdog command = (%#v, %v)", command, err)
	}
}

func TestUnixOwnerTerminateAndWait(t *testing.T) {
	api := systemUnixAPI()
	owner := &unixOwner{pid: 0, api: api}
	if err := owner.terminate(); err != nil {
		t.Fatalf("zero termination error = %v", err)
	}
	api.kill = func(int, syscall.Signal) error { return syscall.ESRCH }
	owner.pid, owner.api = 41, api
	terminateErr := owner.terminate()
	waitErr := owner.wait(api.now().Add(time.Second))
	if terminateErr != nil || waitErr != nil {
		t.Fatalf("missing group errors = (%v, %v)", terminateErr, waitErr)
	}
	failure := errors.New("kill")
	api.kill = func(int, syscall.Signal) error { return failure }
	owner = &unixOwner{pid: 41, api: api}
	if err := owner.terminate(); !errors.Is(err, failure) {
		t.Fatalf("termination error = %v", err)
	}
	if err := owner.wait(api.now().Add(time.Second)); !errors.Is(err, failure) {
		t.Fatalf("wait error = %v", err)
	}
	probes := 0
	api.kill = func(int, syscall.Signal) error {
		probes++
		if probes == 1 {
			return nil
		}
		return syscall.ESRCH
	}
	slept := false
	api.sleep = func(time.Duration) { slept = true }
	owner.api = api
	if err := owner.wait(api.now().Add(time.Second)); err != nil || !slept {
		t.Fatalf("eventual wait = (%v, %t)", err, slept)
	}
	api.kill = func(int, syscall.Signal) error { return nil }
	now := time.Unix(1, 0)
	api.now = func() time.Time { now = now.Add(time.Second); return now }
	owner.api = api
	if err := owner.wait(now.Add(time.Nanosecond)); err == nil {
		t.Fatal("wait deadline accepted")
	}
}

func TestUnixOwnerDoesNotRepeatSuccessfulTermination(t *testing.T) {
	api := systemUnixAPI()
	calls := 0
	api.kill = func(int, syscall.Signal) error {
		calls++
		if calls > 1 {
			return syscall.EPERM
		}
		return nil
	}
	owner := &unixOwner{pid: 41, api: api}
	if first, second := owner.terminate(), owner.terminate(); first != nil || second != nil || calls != 1 {
		t.Fatalf("termination = (%v, %v, %d calls)", first, second, calls)
	}
}

func TestUnixOwnerRetriesPermissionProbeUntilAbsence(t *testing.T) {
	api := systemUnixAPI()
	probes := 0
	api.kill = func(int, syscall.Signal) error {
		probes++
		if probes < 3 {
			return syscall.EPERM
		}
		return syscall.ESRCH
	}
	api.sleep = func(time.Duration) {}
	owner := &unixOwner{pid: 41, api: api}
	if err := owner.wait(api.now().Add(time.Second)); err != nil {
		t.Fatalf("eventual absence error = %v", err)
	}
	if probes != 3 {
		t.Fatalf("probes = %d", probes)
	}
}

func TestUnixOwnerRejectsPersistentPermissionProbe(t *testing.T) {
	api := systemUnixAPI()
	api.kill = func(int, syscall.Signal) error { return syscall.EPERM }
	now := time.Unix(1, 0)
	api.now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	owner := &unixOwner{pid: 41, api: api}
	if err := owner.wait(now.Add(time.Nanosecond)); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("persistent permission error = %v", err)
	}
}

func TestUnixOwnerStopWatchdog(t *testing.T) {
	failure := errors.New("watchdog")
	done := make(chan struct{})
	close(done)
	owner := &unixOwner{api: systemUnixAPI(), watchdogDone: done}
	if err := owner.close(time.Now().Add(DefaultCleanupTimeout)); err == nil {
		t.Fatal("clean watchdog exit accepted")
	}
	owner = &unixOwner{api: systemUnixAPI(), watchdogDone: done, watchdogErr: failure}
	if err := owner.close(time.Now().Add(DefaultCleanupTimeout)); !errors.Is(err, failure) {
		t.Fatalf("watchdog error = %v", err)
	}
	api := systemUnixAPI()
	immediate := make(chan time.Time, 1)
	immediate <- time.Now()
	api.timer = func(time.Duration) ownerTimer { return &fixtureTimer{events: immediate} }
	done = make(chan struct{})
	api.kill = func(int, syscall.Signal) error { close(done); return nil }
	owner = &unixOwner{pid: 41, api: api, watchdogDone: done, watchdogErr: failure}
	if err := owner.close(api.now().Add(time.Second)); !errors.Is(err, failure) {
		t.Fatalf("forced watchdog error = %v", err)
	}
	first := make(chan time.Time, 1)
	first <- time.Now()
	second := make(chan time.Time, 1)
	second <- time.Now()
	calls := 0
	api.timer = func(time.Duration) ownerTimer {
		calls++
		if calls == 1 {
			return &fixtureTimer{events: first}
		}
		return &fixtureTimer{events: second}
	}
	api.kill = func(int, syscall.Signal) error { return failure }
	owner = &unixOwner{pid: 41, api: api, watchdogDone: make(chan struct{})}
	if err := owner.close(api.now().Add(time.Second)); err == nil {
		t.Fatal("silent watchdog accepted")
	}
}

func TestWatchdogWaitError(t *testing.T) {
	if err := watchdogWaitError(nil); err == nil {
		t.Fatal("clean exit accepted")
	}
	failure := errors.New("wait")
	if err := watchdogWaitError(failure); !errors.Is(err, failure) {
		t.Fatalf("ordinary wait error = %v", err)
	}
	if err := watchdogWaitError(exec.CommandContext(t.Context(), "sh", "-c", "exit 1").Run()); err == nil {
		t.Fatal("exit status accepted")
	}
	if err := watchdogWaitError(exec.CommandContext(t.Context(), "sh", "-c", "kill -9 $$").Run()); err != nil {
		t.Fatalf("SIGKILL error = %v", err)
	}
}

func TestWatchdogAfterDeadlineRechecksCompletion(t *testing.T) {
	done := make(chan struct{})
	close(done)
	waitErr := exec.CommandContext(t.Context(), "sh", "-c", "kill -9 $$").Run()
	owner := &unixOwner{watchdogDone: done, watchdogErr: waitErr}
	if err := owner.watchdogAfterDeadline(nil); err != nil {
		t.Fatal(err)
	}
	owner = &unixOwner{watchdogDone: make(chan struct{})}
	if err := owner.watchdogAfterDeadline(nil); err == nil {
		t.Fatal("unfinished watchdog accepted")
	}
}

func TestWatchdogDeadlineConcurrentExit(t *testing.T) {
	for range 1_000 {
		owner := &unixOwner{watchdogDone: make(chan struct{})}
		go owner.recordWatchdogExit(errors.New("unexpected"))
		if err := owner.watchdogAfterDeadline(nil); err == nil {
			t.Fatal("watchdog deadline accepted an incomplete exit")
		}
		<-owner.watchdogDone
		if err := owner.watchdogAfterDeadline(nil); !errors.Is(err, ErrOwnership) {
			t.Fatalf("watchdog exit error = %v", err)
		}
	}
}

func TestObservedWatchdogLossIsNotRepeatedDuringCleanup(t *testing.T) {
	owner := &unixOwner{watchdogErr: errors.New("unexpected"), watchdogLost: true, healthSeen: true}
	if err := owner.watchdogCleanupError(); err != nil {
		t.Fatal(err)
	}
}

func TestStopTimer(t *testing.T) {
	events := make(chan time.Time, 1)
	events <- time.Now()
	first := &fixtureTimer{events: events}
	second := &fixtureTimer{events: make(chan time.Time), stopped: true}
	stopTimer(first)
	stopTimer(second)
	if first.stopCalls != 1 || second.stopCalls != 1 || first.channelCalls == 0 {
		t.Fatalf("timer calls = first %#v, second %#v", first, second)
	}
}

func cleanupCommand() Command { return Command{CleanupTimeout: DefaultCleanupTimeout} }

type fixtureTimer struct {
	events       <-chan time.Time
	stopped      bool
	stopCalls    int
	channelCalls int
}

func (timer *fixtureTimer) channel() <-chan time.Time {
	timer.channelCalls++
	return timer.events
}

func (timer *fixtureTimer) stop() bool {
	timer.stopCalls++
	return timer.stopped
}
