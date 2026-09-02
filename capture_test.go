package proctree

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOutputCaptureConstructionFailures(t *testing.T) {
	t.Parallel()
	failure := errors.New("pipe")
	if _, err := newOutputCaptureWith(1, 1, make(chan struct{}), &sync.Once{},
		func() (*os.File, *os.File, error) { return nil, nil, failure }, closeFile); !errors.Is(err, failure) {
		t.Fatalf("first pipe error = %v", err)
	}
	calls := 0
	pipe := func() (*os.File, *os.File, error) {
		calls++
		if calls == 2 {
			return nil, nil, failure
		}
		return os.Pipe()
	}
	closeFailure := errors.New("close")
	close := func(file *os.File) error { return errors.Join(closeFile(file), closeFailure) }
	if _, err := newOutputCaptureWith(1, 1, make(chan struct{}), &sync.Once{}, pipe, close); !errors.Is(err, failure) || !errors.Is(err, closeFailure) || !errors.Is(err, ErrCleanup) {
		t.Fatalf("second pipe error = %v", err)
	}
}

func TestCaptureOutputFailures(t *testing.T) {
	t.Parallel()
	failure := errors.New("read")
	if err := captureOutput(errorReader{err: failure}, io.Discard); !errors.Is(err, failure) {
		t.Fatalf("read error = %v", err)
	}
	if err := captureOutput(strings.NewReader("value"), errorWriter{err: failure}); !errors.Is(err, failure) {
		t.Fatalf("write error = %v", err)
	}
	if err := captureError(failure); !errors.Is(err, failure) {
		t.Fatalf("capture error = %v", err)
	}
}

func TestCaptureCloseHelpers(t *testing.T) {
	t.Parallel()
	if err := closeFile(nil); err != nil {
		t.Fatalf("nil close error = %v", err)
	}
	file, err := os.CreateTemp(t.TempDir(), "closed-")
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = closeFile(file); err != nil {
		t.Fatalf("repeated close error = %v", err)
	}
}

func TestOutputCaptureDrainsBothStreamsBeforeClosingReaders(t *testing.T) {
	overflow := make(chan struct{})
	capture, err := newOutputCapture(8, 8, overflow, &sync.Once{})
	if err != nil {
		t.Fatal(err)
	}
	capture.start()
	value := []byte(strings.Repeat("x", 64))
	if _, err = capture.stdout.write.Write(value); err != nil {
		t.Fatal(err)
	}
	if _, err = capture.stderr.write.Write(value); err != nil {
		t.Fatal(err)
	}
	if joined, finishErr := capture.finish(time.Now().Add(DefaultCleanupTimeout)); !joined || finishErr != nil {
		t.Fatalf("finish = (%t, %v)", joined, finishErr)
	}
	if len(capture.stdout.buffer.bytes()) != 8 || len(capture.stderr.buffer.bytes()) != 8 {
		t.Fatalf("captured lengths = %d, %d", len(capture.stdout.buffer.bytes()), len(capture.stderr.buffer.bytes()))
	}
}

func TestOutputCaptureReaderCloseUnblocksCapture(t *testing.T) {
	capture, err := newOutputCapture(8, 8, make(chan struct{}), &sync.Once{})
	if err != nil {
		t.Fatal(err)
	}
	capture.start()
	if err = capture.closeReaders(); err != nil {
		t.Fatal(err)
	}
	if joined, finishErr := capture.finish(time.Now().Add(DefaultCleanupTimeout)); !joined || finishErr != nil {
		t.Fatalf("finish = (%t, %v)", joined, finishErr)
	}
}

func TestWaitCapturedStreamReceivesBeforeDeadline(t *testing.T) {
	done := make(chan error, 1)
	done <- nil
	if err, joined := waitCapturedStreamTimer(done, make(chan time.Time)); err != nil || !joined {
		t.Fatalf("waitCapturedStreamTimer = (%v, %t)", err, joined)
	}
}

func TestCapturedAfterDeadlineRechecksCompletion(t *testing.T) {
	done := make(chan error, 1)
	done <- nil
	if err, joined := capturedAfterDeadline(done); err != nil || !joined {
		t.Fatalf("capturedAfterDeadline = (%v, %t)", err, joined)
	}
}

type errorReader struct{ err error }

func (reader errorReader) Read([]byte) (int, error) { return 0, reader.err }

type errorWriter struct{ err error }

func (writer errorWriter) Write([]byte) (int, error) { return 0, writer.err }
