package main

import (
	"fmt"
	"io"
	"os"

	"github.com/secengcommons/proctree/tools/workflow/internal/workflowpolicy"
)

func main() { os.Exit(execute(os.Args[1:], os.Stderr)) }

func execute(paths []string, stderr io.Writer) int {
	if err := workflowpolicy.Inspect(paths); err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 2
		}
		return 1
	}
	return 0
}
