//go:build !windows

package proctree

import "os"

func capturePipe() (*os.File, *os.File, error) { return os.Pipe() }
