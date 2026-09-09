//go:build darwin || dragonfly || freebsd || illumos || netbsd || openbsd || solaris

package proctree

import "time"

func processGroupTerminated(int, time.Time) (bool, error) { return false, nil }
