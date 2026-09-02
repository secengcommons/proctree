package proctree

import (
	"os"
	"testing"
	"time"
)

var benchmarkCommand Command

const benchmarkOutputLimit = 4 << 10

func BenchmarkAdmit(b *testing.B) {
	command := benchmarkAdmitCommand()
	for b.Loop() {
		admitted, err := admit(command)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkCommand = admitted
	}
}

func TestAdmitAllocationBound(t *testing.T) {
	command := benchmarkAdmitCommand()
	var operationErr error
	if allocations := testing.AllocsPerRun(100, func() {
		_, operationErr = admit(command)
	}); allocations != 2 {
		t.Fatalf("admit allocations = %f", allocations)
	}
	if operationErr != nil {
		t.Fatal(operationErr)
	}
}

func benchmarkAdmitCommand() Command {
	return Command{
		Executable: os.Args[0], Arguments: []string{"first", "second", "third"},
		Environment: []string{"PATH=value", "HOME=value", "TEMP=value"},
		StdoutLimit: benchmarkOutputLimit, StderrLimit: benchmarkOutputLimit, Timeout: time.Second,
	}
}
