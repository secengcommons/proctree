package proctree

import (
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

const captureBufferBytes = 32 << 10

type outputCapture struct {
	stdout capturedStream
	stderr capturedStream
}

type capturedStream struct {
	read   *os.File
	write  *os.File
	buffer boundedBuffer
	done   chan error
}

func newOutputCapture(stdoutLimit, stderrLimit int, overflow chan struct{}, once *sync.Once) (*outputCapture, error) {
	return newOutputCaptureWith(stdoutLimit, stderrLimit, overflow, once, capturePipe, closeFile)
}

func newOutputCaptureWith(
	stdoutLimit, stderrLimit int,
	overflow chan struct{},
	once *sync.Once,
	pipe func() (*os.File, *os.File, error),
	close func(*os.File) error,
) (*outputCapture, error) {
	stdout, err := newCapturedStream(stdoutLimit, overflow, once, pipe)
	if err != nil {
		return nil, err
	}
	stderr, err := newCapturedStream(stderrLimit, overflow, once, pipe)
	if err != nil {
		return nil, errors.Join(err, cleanupError(errors.Join(close(stdout.read), close(stdout.write))))
	}
	return &outputCapture{stdout: stdout, stderr: stderr}, nil
}

func newCapturedStream(
	limit int,
	overflow chan struct{},
	once *sync.Once,
	pipe func() (*os.File, *os.File, error),
) (capturedStream, error) {
	read, write, err := pipe()
	if err != nil {
		return capturedStream{}, err
	}
	return capturedStream{
		read: read, write: write, buffer: boundedBuffer{limit: limit, overflow: overflow, once: once}, done: make(chan error, 1),
	}, nil
}

func (capture *outputCapture) start() {
	go capture.stdout.capture()
	go capture.stderr.capture()
}

func (stream *capturedStream) capture() { stream.done <- captureOutput(stream.read, &stream.buffer) }

func captureOutput(reader io.Reader, output io.Writer) error {
	var buffer [captureBufferBytes]byte
	for {
		count, err := reader.Read(buffer[:])
		if count > 0 {
			if _, writeErr := output.Write(buffer[:count]); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (capture *outputCapture) closeWriters() error {
	return errors.Join(closeFile(capture.stdout.write), closeFile(capture.stderr.write))
}

func (capture *outputCapture) closeReaders() error {
	return errors.Join(closeFile(capture.stdout.read), closeFile(capture.stderr.read))
}

func (capture *outputCapture) finish(deadline time.Time) (bool, error) {
	writeErr := capture.closeWriters()
	stdoutErr, stdoutJoined := waitCapturedStream(capture.stdout.done, deadline)
	stderrErr, stderrJoined := waitCapturedStream(capture.stderr.done, deadline)
	readErr := capture.closeReaders()
	joined := stdoutJoined && stderrJoined
	var deadlineErr error
	if !joined {
		deadlineErr = errors.New("process output did not close before cleanup deadline")
	}
	return joined, errors.Join(writeErr, stdoutErr, stderrErr, readErr, deadlineErr)
}

func waitCapturedStream(done <-chan error, deadline time.Time) (error, bool) {
	select {
	case err := <-done:
		return captureError(err), true
	default:
	}
	timer := time.NewTimer(max(0, time.Until(deadline)))
	defer timer.Stop()
	return waitCapturedStreamTimer(done, timer.C)
}

func waitCapturedStreamTimer(done <-chan error, timer <-chan time.Time) (error, bool) {
	select {
	case err := <-done:
		return captureError(err), true
	case <-timer:
		return capturedAfterDeadline(done)
	}
}

func capturedAfterDeadline(done <-chan error) (error, bool) {
	select {
	case err := <-done:
		return captureError(err), true
	default:
		return nil, false
	}
}

func closeFile(file *os.File) error {
	if file == nil {
		return nil
	}
	err := file.Close()
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

func captureError(err error) error {
	if errors.Is(err, os.ErrClosed) || errors.Is(err, ErrOutputLimit) {
		return nil
	}
	return err
}
