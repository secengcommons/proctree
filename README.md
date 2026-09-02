<div align="center">
  <h1>Security Engineering Commons Proctree</h1>
  Bounded process-tree execution, cancellation and cleanup for Go
</div>
<br>

Proctree executes a trusted local command and returns after its owned process tree terminates or cleanup fails. Proctree is not a privilege boundary or hostile-code sandbox

## API

`Run(context.Context, Command)` accepts an absolute executable, arguments, working directory, closed environment, optional input, output limits and execution deadlines

`Result` includes start state, exit code, bounded output and a typed outcome. Errors preserve sentinel and native causes

Unix executables using `Run` must call `DispatchWatchdog(os.Args)` before ordinary dispatch

The complete guarantees and bounds are in the [contract](docs/contract.md)

## Platforms

CI runs the supported Windows, Linux, macOS, BSD, illumos and Solaris targets listed in [Platforms](docs/platforms.md)

## Verification
```text
go test -count=1 ./...
go test -race -count=1 ./...
```

CI also covers Go compatibility, cross-builds, containers, fuzzing and CodeQL
