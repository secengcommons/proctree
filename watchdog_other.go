//go:build !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package proctree

func DispatchWatchdog([]string) (bool, int) { return false, 0 }
