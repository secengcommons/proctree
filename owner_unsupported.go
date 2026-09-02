//go:build !windows && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package proctree

func newProcessOwner() (processOwner, error) { return nil, ErrUnsupported }
