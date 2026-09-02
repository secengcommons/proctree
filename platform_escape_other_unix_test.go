//go:build darwin || dragonfly || freebsd || illumos || netbsd || openbsd || solaris

package proctree

func runEscapedWriterHelper(string) bool { return false }
