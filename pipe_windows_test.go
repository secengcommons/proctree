//go:build windows

package proctree

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPipeFailures(t *testing.T) {
	t.Parallel()
	failure := errors.New("pipe")

	api := systemWindowsPipeAPI()
	api.name = func() (string, error) { return "", failure }
	assertWindowsPipeFailure(t, api, failure)

	api = systemWindowsPipeAPI()
	api.name = func() (string, error) { return "invalid\x00name", nil }
	if _, _, err := windowsPipeWith(api, windows.PIPE_ACCESS_INBOUND, windows.GENERIC_WRITE); err == nil {
		t.Fatal("invalid pipe name accepted")
	}

	api = systemWindowsPipeAPI()
	api.create = func(*uint16, uint32) (windows.Handle, error) { return windows.InvalidHandle, failure }
	assertWindowsPipeFailure(t, api, failure)

	api = systemWindowsPipeAPI()
	api.open = func(*uint16, uint32) (windows.Handle, error) { return windows.InvalidHandle, failure }
	assertWindowsPipeFailure(t, api, failure)
}

func assertWindowsPipeFailure(t *testing.T, api windowsPipeAPI, failure error) {
	t.Helper()
	if _, _, err := windowsPipeWith(api, windows.PIPE_ACCESS_INBOUND, windows.GENERIC_WRITE); !errors.Is(err, failure) {
		t.Fatalf("pipe error = %v", err)
	}
}
