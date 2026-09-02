//go:build windows

package proctree

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

const localPipePrefix = `\\.\pipe\secengcommons-proctree-`

type windowsPipeAPI struct {
	name   func() (string, error)
	create func(*uint16, uint32) (windows.Handle, error)
	open   func(*uint16, uint32) (windows.Handle, error)
}

func systemWindowsPipeAPI() windowsPipeAPI {
	return windowsPipeAPI{
		name: localPipeName,
		create: func(name *uint16, access uint32) (windows.Handle, error) {
			// Overlapped parent operations let Close cancel input and output held by a child
			return windows.CreateNamedPipe(
				name,
				access|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
				windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
				1,
				captureBufferBytes,
				captureBufferBytes,
				0,
				nil,
			)
		},
		open: func(name *uint16, access uint32) (windows.Handle, error) {
			return windows.CreateFile(
				name,
				access,
				0,
				nil,
				windows.OPEN_EXISTING,
				windows.FILE_ATTRIBUTE_NORMAL|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_ANONYMOUS,
				0,
			)
		},
	}
}

func localPipeName() (string, error) {
	identifier, err := windows.GenerateGUID()
	return localPipePrefix + identifier.String(), err
}

func capturePipe() (*os.File, *os.File, error) {
	return windowsPipeWith(systemWindowsPipeAPI(), windows.PIPE_ACCESS_INBOUND, windows.GENERIC_WRITE)
}

func inputPipe() (*os.File, *os.File, error) {
	write, read, err := windowsPipeWith(systemWindowsPipeAPI(), windows.PIPE_ACCESS_OUTBOUND, windows.GENERIC_READ)
	return read, write, err
}

func windowsPipeWith(api windowsPipeAPI, serverAccess, clientAccess uint32) (*os.File, *os.File, error) {
	name, err := api.name()
	if err != nil {
		return nil, nil, fmt.Errorf("create local pipe name: %w", err)
	}
	windowsName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, nil, fmt.Errorf("encode local pipe name: %w", err)
	}
	serverHandle, err := api.create(windowsName, serverAccess)
	if err != nil {
		return nil, nil, fmt.Errorf("create local pipe: %w", err)
	}
	server := os.NewFile(uintptr(serverHandle), name)
	clientHandle, err := api.open(windowsName, clientAccess)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("open local pipe: %w", err), cleanupError(closeFile(server)))
	}
	return server, os.NewFile(uintptr(clientHandle), name), nil
}
