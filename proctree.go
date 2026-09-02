package proctree

import (
	"errors"
	"time"
)

const MaxArguments = 256
const MaxArgumentBytes = 64 << 10
const MaxEnvironment = 128
const MaxEnvironmentBytes = 64 << 10
const MaxInputBytes = 1 << 20
const MaxOutputBytes = 16 << 20
const MaxPathBytes = 4 << 10
const MaxTimeout = 30 * time.Minute
const MaxCleanupTimeout = 30 * time.Second
const DefaultCleanupTimeout = time.Second

var ErrInvalid = errors.New("invalid process command")
var ErrUnsupported = errors.New("process-tree ownership is unsupported")
var ErrStart = errors.New("process start failed")
var ErrOwnership = errors.New("process ownership failed")
var ErrExit = errors.New("process exited unsuccessfully")
var ErrCancelled = errors.New("process execution cancelled")
var ErrDeadline = errors.New("process execution deadline exceeded")
var ErrOutputLimit = errors.New("process output exceeds its bound")
var ErrCleanup = errors.New("process cleanup failed")

type Outcome string

const OutcomeInvalid Outcome = "invalid"
const OutcomeCompleted Outcome = "completed"
const OutcomeExitFailure Outcome = "exit_failure"
const OutcomeCancelled Outcome = "cancelled"
const OutcomeDeadline Outcome = "deadline"
const OutcomeOutputLimit Outcome = "output_limit"
const OutcomeStartFailure Outcome = "start_failure"
const OutcomeOwnershipFailure Outcome = "ownership_failure"
const OutcomeCleanupFailure Outcome = "cleanup_failure"
const OutcomeUnsupported Outcome = "unsupported"

type Command struct {
	Executable     string
	Arguments      []string
	Directory      string
	Environment    []string
	Input          []byte
	StdoutLimit    int
	StderrLimit    int
	Timeout        time.Duration
	CleanupTimeout time.Duration
}

type Result struct {
	Started  bool
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Outcome  Outcome
}
