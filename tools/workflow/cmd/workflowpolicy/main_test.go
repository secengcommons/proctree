package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

const maxWorkflowFixtureBytes = 64 << 10

func TestExecute(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "..", "..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := execute(paths, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("valid execution = (%d, %q)", code, stderr.String())
	}
	fixture, fixturePaths := copyWorkflowSet(t, paths)
	invalid := filepath.Join(fixture, "core.yml")
	if err := os.WriteFile(invalid, []byte("uses: actions/checkout@main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := execute(fixturePaths, &stderr); code != 1 || !bytes.Contains(stderr.Bytes(), []byte(strconv.Quote(invalid))) {
		t.Fatalf("complete path execution = (%d, %q)", code, stderr.String())
	}
	if code := execute(nil, &stderr); code != 1 || stderr.Len() == 0 {
		t.Fatalf("invalid execution = (%d, %q)", code, stderr.String())
	}
	if code := execute(nil, failingWriter{}); code != 2 {
		t.Fatalf("failed error output = %d", code)
	}
}

func copyWorkflowSet(t *testing.T, paths []string) (string, []string) {
	t.Helper()
	directory := t.TempDir()
	copies := make([]string, 0, len(paths))
	for _, path := range paths {
		destination := filepath.Join(directory, filepath.Base(path))
		source := readOwnedTestFile(t, path)
		writeOwnedTestFile(t, destination, source)
		copies = append(copies, destination)
	}
	return directory, copies
}

func readOwnedTestFile(t *testing.T, path string) []byte {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		t.Fatal(errors.Join(err, root.Close()))
	}
	source, readErr := io.ReadAll(io.LimitReader(file, maxWorkflowFixtureBytes+1))
	if readErr == nil && len(source) > maxWorkflowFixtureBytes {
		readErr = errors.New("workflow fixture exceeds its bound")
	}
	if err := errors.Join(readErr, file.Close(), root.Close()); err != nil {
		t.Fatal(err)
	}
	return source
}

func writeOwnedTestFile(t *testing.T, path string, source []byte) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	file, err := root.OpenFile(filepath.Base(path), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(errors.Join(err, root.Close()))
	}
	written, writeErr := file.Write(source)
	if writeErr == nil && written != len(source) {
		writeErr = io.ErrShortWrite
	}
	if err := errors.Join(writeErr, file.Close(), root.Close()); err != nil {
		t.Fatal(err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write") }
