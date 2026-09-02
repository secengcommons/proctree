## Purpose

`Run` executes one trusted local command and returns after its owned process tree terminates or cleanup fails

## Command

Field | Contract
--- | ---
`Executable` | Exact absolute path
`Arguments` | Argument vector without shell interpretation
`Directory` | Empty inherits the controller directory; otherwise an exact absolute path which is not root-confined
`Environment` | Closed environment; nil or empty means no variables; names match `[A-Za-z_][A-Za-z0-9_]*` and duplicate comparison follows native case semantics
`Input` | Copied before start; nil means immediate end-of-file
`Timeout` | Relative deadline; zero uses only the caller context
`CleanupTimeout` | Cleanup deadline; zero selects the default

Invalid UTF-8, NUL bytes, invalid environment entries, relative executables, negative durations and excessive material fail before start

## Bounds

Material | Maximum
--- | ---:
Arguments | 256
Argument bytes | 64 KiB
Environment entries | 128
Environment bytes | 64 KiB
Input | 1 MiB
Standard output | 16 MiB
Standard error | 16 MiB
Relative timeout | 30 minutes
Cleanup timeout | 30 seconds
Path | 4 KiB

An output stream may reach its exact limit. The next byte terminates the tree and returns both bounded output prefixes

## Execution

Context cancellation and the relative deadline are checked after each synchronous startup call. Cancellation then terminates and joins the owned tree before result selection

Native owner creation and process startup cannot be interrupted while an operating-system call is blocked

`Started` becomes true after Proctree releases its operating-system start barrier. An external Windows suspension owner may still delay execution. `ExitCode` is `-1` without a joined primary process state

Process ownership, wait and output capture share one absolute cleanup deadline. A child retaining an output handle beyond that deadline produces `cleanup_failure`; partial output is omitted when capture cannot be joined

Unexpected watchdog exit is an ownership failure which terminates and joins the target group

Errors retain sentinel identity and native cause. Cleanup failure remains joined with any primary failure

Cleanup failure is the final typed outcome. Otherwise an observed cancellation or deadline remains the typed outcome when output overflow is concurrent

When cancellation or deadline coincides with watchdog loss, the outcome remains cancelled or deadline and the error also includes ownership failure

Outcome | Meaning
--- | ---
`invalid` | Invalid command or context
`completed` | Zero exit with complete cleanup
`exit_failure` | Non-zero exit
`cancelled` | Caller cancellation
`deadline` | Caller or relative deadline
`output_limit` | Output exceeded its bound
`start_failure` | Start failed
`ownership_failure` | Process-tree ownership failed before or during execution
`cleanup_failure` | Termination, wait or close failed
`unsupported` | No qualified platform owner

## Watchdog

Unix executables using `Run` must call `DispatchWatchdog(os.Args)` before ordinary dispatch

Process-group termination requires all of these conditions:
- The watchdog leads its new process group
- The control descriptor is a pipe
- The readiness descriptor is a pipe
- The controller owns the pipe until close or exit

The target starts only after the watchdog validates its descriptors and process-group leadership then sends the exact readiness byte and closes the readiness descriptor

Readiness and failed-start cleanup share `CleanupTimeout`

Direct invocation without these conditions fails without signalling a process group

## Compatibility

The minimum Go version is 1.24

Windows execution requires Windows 10 or Windows Server 2016 and newer
