<div align="center">
  <h1>Security Engineering Commons Proctree</h1>
  Bounded process-tree execution, cancellation and cleanup for Go
</div>
<br>

Proctree executes a trusted local command and returns after its owned process tree terminates or cleanup fails. It is not a privilege boundary or hostile-code sandbox

Commands use an absolute executable path and an argument vector without shell interpretation. Input, output, environment material and execution time are bounded

## Summary
- [Install](#install)
- [Use](#use)
- [Bounds](#bounds)
- [Outcomes](#outcomes)
- [Platforms](#platforms)
- [Performance](#performance)
  - [Benchmark method](#benchmark-method)
  - [Benchmark results](#benchmark-results)
- [Boundary](#boundary)
- [Verification](#verification)

## Install
```sh
go get github.com/secengcommons/proctree@v1.0.0
```

Requires Go 1.26 or newer

## Use
```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/secengcommons/proctree"
)

func main() {
	if handled, code := proctree.DispatchWatchdog(os.Args); handled {
		os.Exit(code)
	}

	result, err := proctree.Run(context.Background(), proctree.Command{
		Executable:  "/usr/bin/git",
		Arguments:   []string{"status", "--short"},
		Environment: []string{"PATH=/usr/bin"},
		StdoutLimit: 64 << 10,
		StderrLimit: 64 << 10,
		Timeout:     10 * time.Second,
	})
	if err != nil {
		panic(err)
	}
	fmt.Print(string(result.Stdout))
}
```

Unix executables must call `DispatchWatchdog(os.Args)` before ordinary command dispatch. Windows uses a kill-on-close Job Object and suspended process creation

`Result` reports whether execution started, the exit code, bounded output and a typed outcome. Errors preserve Proctree sentinel errors and native causes

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
Execution timeout | 30 minutes
Cleanup timeout | 30 seconds
Path | 4 KiB

Nil or empty environment means no environment variables. Zero execution timeout leaves timing to the context. Zero cleanup timeout selects the one-second default

An output stream may reach its exact limit. The next byte terminates the tree and returns the bounded output prefixes

## Outcomes

Outcome | Meaning
--- | ---
invalid | Invalid command or context
completed | Zero exit with complete cleanup
exit_failure | Non-zero exit
cancelled | Caller cancellation
deadline | Caller or command deadline
output_limit | Output exceeded its bound
start_failure | Process creation failed
ownership_failure | Process-tree ownership failed
cleanup_failure | Termination, wait or close failed
unsupported | No qualified platform owner

Cleanup failure takes precedence over an earlier execution outcome. Native owner creation and process startup cannot be interrupted while an operating-system call is blocked

## Platforms

Platform | Qualified execution
--- | ---
Windows | Server 2022 and 2025 AMD64; Windows 11 ARM64
Linux | Ubuntu 22.04 and 24.04 on AMD64 and ARM64
macOS | Versions 15 and 26 on AMD64 and ARM64
FreeBSD | Versions 13.5, 14.4 and 15.1 on AMD64
OpenBSD | Versions 7.7, 7.8 and 7.9 on AMD64
NetBSD | Versions 9.4, 10.1 and 11.0 on AMD64
DragonFly BSD | Version 6.4.2 on AMD64
illumos | OmniOS r151058 on AMD64
Solaris | Version 11.4 on AMD64

AIX and other Go ports are compile-only or unsupported. Unsupported targets refuse execution rather than falling back to immediate-process-only termination

Windows requires Windows 10 or Windows Server 2016 and later. A parent Job must permit nested Job ownership

## Performance

Command admission is measured independently from operating-system process creation

### Benchmark method

(9 September 2026) - The measurements use:
- Linux AMD64
- 13th Gen Intel Core i5-13400F
- Go 1.27.1
- five 500 ms samples per operation
- the median of each five-sample set

### Benchmark results

Operation | Time | Bytes | Allocations
--- | ---: | ---: | ---:
Admit one command with three arguments and three environment entries | 225.5 ns | 96 | 2

The benchmark includes two output limits and one timeout. It does not start a process. Run the complete set with:
```sh
go test -run '^$' -bench . -benchmem -benchtime=500ms -count=5 ./...
```

## Boundary

Unix ownership covers descendants which remain in the inherited process group. Container ownership is limited to the caller's PID namespace. Deliberate process-group, session or namespace escape is not covered

Proctree bounds input, output and execution. It does not validate the executable, provide isolation or prevent descendants from using ambient authority

## Verification
```text
go test -count=1 ./...
go test -race -count=1 ./...
```

CI also runs compatibility, coverage, fuzz, container, native-platform, virtual-platform, cross-build and CodeQL checks
