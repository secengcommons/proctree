#!/usr/bin/env bash
set -euo pipefail

repository_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
readonly repository_root
temporary=$(mktemp -d "${TMPDIR:-/tmp}/secengcommons-proctree-verification.XXXXXX")
case "$temporary" in
  "${TMPDIR:-/tmp}"/secengcommons-proctree-verification.*) ;;
  *) printf 'Unsafe verification directory: %s\n' "$temporary" >&2; exit 1 ;;
esac
trap 'rm -rf -- "$temporary"' EXIT
real_go=$(command -v go)
[[ -x "$real_go" ]]

test_omitting_workflow_command() {
  local root=$temporary/truncating
  mkdir -p -- "$root/.github/scripts" "$root/.github/workflows" "$root/tools"
  cp -- "$repository_root/.github/scripts/check-workflows.sh" "$root/.github/scripts/"
  cp -- "$repository_root/.github/workflows/"*.yml "$root/.github/workflows/"
  cp -R -- "$repository_root/tools/workflow" "$root/tools/workflow"
  cat >"$root/tools/workflow/cmd/workflowpolicy/main.go" <<'EOF'
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/secengcommons/proctree/tools/workflow/internal/workflowpolicy"
)

func main() {
	paths := os.Args[1:]
	selected := paths[:0]
	for _, path := range paths {
		if filepath.Base(path) != "core.yml" {
			selected = append(selected, path)
		}
	}
	os.Exit(execute(selected, os.Stderr))
}

func execute(paths []string, stderr io.Writer) int {
	if err := workflowpolicy.Inspect(paths); err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 2
		}
		return 1
	}
	return 0
}
EOF
  for trace in normal xtrace; do
    command=(bash)
    [[ "$trace" == xtrace ]] && command+=(-x)
    if "${command[@]}" "$root/.github/scripts/check-workflows.sh" >"$root/$trace.stdout" 2>"$root/$trace.stderr"; then
      printf 'Workflow driver accepted a name-omitting executable under %s execution\n' "$trace" >&2
      return 1
    fi
  done
}

test_partial_workflow_discovery() {
  local bin=$temporary/workflow-bin
  mkdir -- "$bin"
  cat >"$bin/find" <<'EOF'
#!/bin/sh
printf '%s\n' "${PARTIAL_WORKFLOW:?}"
exit 1
EOF
  chmod 0700 "$bin/find"
  local output
  if output=$(PARTIAL_WORKFLOW="$repository_root/.github/workflows/core.yml" PATH="$bin:$PATH" \
    bash "$repository_root/.github/scripts/check-workflows.sh" 2>&1); then
    printf 'Workflow discovery accepted partial output\n' >&2
    return 1
  fi
  grep -Fq 'Workflow discovery failed' <<<"$output"
}

test_partial_diagnostic_size() {
  local bin=$temporary/size-bin
  mkdir -- "$bin"
  cat >"$bin/wc" <<'EOF'
#!/bin/sh
printf '10\n'
exit 1
EOF
  chmod 0700 "$bin/wc"
  local output
  if output=$(PATH="$bin:$PATH" bash "$repository_root/.github/scripts/check-workflows.sh" 2>&1); then
    printf 'Diagnostic sizing accepted partial output\n' >&2
    return 1
  fi
  grep -Fq 'Diagnostic size inspection failed' <<<"$output"
}

test_partial_go_target_discovery() {
  local bin=$temporary/target-bin
  local sentinel=$temporary/later-go-call
  mkdir -- "$bin"
  cat >"$bin/go" <<'EOF'
#!/bin/sh
if [ "$#" -eq 3 ] && [ "$1" = tool ] && [ "$2" = dist ] && [ "$3" = list ]; then
  printf 'linux/amd64\n'
  exit 1
fi
: >"${LATER_GO_CALLED:?}"
exec "${REAL_GO:?}" "$@"
EOF
  chmod 0700 "$bin/go"
  local output
  if output=$(LATER_GO_CALLED="$sentinel" REAL_GO="$real_go" PATH="$bin:$PATH" \
    bash "$repository_root/.github/scripts/build-targets.sh" standard 2>&1); then
    printf 'Go target discovery accepted partial output\n' >&2
    return 1
  fi
  grep -Fq 'Go target discovery failed' <<<"$output"
  [[ ! -e "$sentinel" ]]
}

test_complete_go_target_discovery() {
  local bin=$temporary/complete-target-bin
  local receipt=$temporary/built-targets
  mkdir -- "$bin"
  cat >"$bin/go" <<'EOF'
#!/bin/sh
if [ "$#" -eq 3 ] && [ "$1" = tool ] && [ "$2" = dist ] && [ "$3" = list ]; then
  printf 'linux/amd64\nwindows/amd64\n'
  exit 0
fi
if [ "${1:-}" = test ] && [ "${2:-}" = -c ]; then
  printf '%s/%s\n' "${GOOS:?}" "${GOARCH:?}" >>"${COMPILED_TARGETS:?}"
fi
exec "${REAL_GO:?}" "$@"
EOF
  chmod 0700 "$bin/go"
  COMPILED_TARGETS="$receipt" REAL_GO="$real_go" PATH="$bin:$PATH" \
    bash "$repository_root/.github/scripts/build-targets.sh" standard
  [[ "$(cat "$receipt")" == $'linux/amd64\nwindows/amd64' ]]
}

test_partial_format_discovery() {
  local bin=$temporary/format-bin
  local sentinel=$temporary/linter-called
  mkdir -- "$bin"
  cat >"$bin/find" <<'EOF'
#!/bin/sh
printf '%s\0' "${PARTIAL_GO_FILE:?}"
exit 1
EOF
  cat >"$bin/linter" <<'EOF'
#!/bin/sh
: >"${LINTER_CALLED:?}"
exit 0
EOF
  chmod 0700 "$bin/find" "$bin/linter"
  local output
  if output=$(LINTER_CALLED="$sentinel" PARTIAL_GO_FILE=tools/workflow/cmd/workflowpolicy/main.go PATH="$bin:$PATH" \
    bash "$repository_root/.github/scripts/check-format.sh" "$bin/linter" 2>&1); then
    printf 'Format discovery accepted partial output\n' >&2
    return 1
  fi
  grep -Fq 'Workflow Go-file discovery failed' <<<"$output"
  [[ ! -e "$sentinel" ]]
}

test_complete_format_execution() {
  local bin=$temporary/complete-format-bin
  local formatter=$temporary/formatter-called
  local linter=$temporary/linter-called
  mkdir -- "$bin"
  cat >"$bin/gofmt" <<'EOF'
#!/usr/bin/env bash
[[ "$1" = -l && "$#" -gt 1 ]]
: >"${FORMATTER_CALLED:?}"
EOF
  cat >"$bin/linter" <<'EOF'
#!/usr/bin/env bash
[[ "$1" = fmt && "$2" = --diff ]]
: >"${LINTER_CALLED:?}"
EOF
  chmod 0700 "$bin/gofmt" "$bin/linter"
  FORMATTER_CALLED="$formatter" LINTER_CALLED="$linter" PATH="$bin:$PATH" \
    bash "$repository_root/.github/scripts/check-format.sh" "$bin/linter"
  [[ -e "$formatter" && -e "$linter" ]]
}

test_complete_fuzz_execution() {
  local module=$temporary/complete-fuzz
  mkdir -- "$module"
  local go_version
  go_version=$(awk '$1 == "go" { print $2; exit }' "$repository_root/go.mod")
  printf 'module fuzz.test/complete\n\ngo %s\n' "$go_version" >"$module/go.mod"
  cat >"$module/fuzz_test.go" <<'EOF'
package complete

import (
	"os"
	"testing"
)

func FuzzComplete(f *testing.F) {
	f.Fuzz(func(*testing.T, []byte) { _ = os.WriteFile("fuzz-ran", nil, 0o600) })
}
EOF
  FUZZTIME=1x bash "$repository_root/.github/scripts/test-fuzz.sh" "$module"
  [[ -e "$module/fuzz-ran" ]]
}

test_partial_tool_resolution() {
  local bin=$temporary/tool-bin
  local output=$temporary/github-output
  mkdir -- "$bin"
  : >"$output"
  cat >"$bin/go" <<'EOF'
#!/bin/sh
printf '/bin/true\n'
exit 1
EOF
  chmod 0700 "$bin/go"
  local diagnostic
  if diagnostic=$(PATH="$bin:$PATH" bash "$repository_root/.github/scripts/resolve-tools.sh" "$output" 2>&1); then
    printf 'Tool resolution accepted partial output\n' >&2
    return 1
  fi
  grep -Fq 'GolangCI-Lint resolution failed' <<<"$diagnostic"
  [[ ! -s "$output" ]]
}

test_aix_refusal_selection() {
  local files
  if ! files=$(env CGO_ENABLED=0 GOOS=aix GOARCH=ppc64 go list -f '{{join .GoFiles " "}}' .); then
    printf 'AIX source selection failed\n' >&2
    return 1
  fi
  [[ " $files " == *' owner_unsupported.go '* ]]
  [[ " $files " != *' owner_unix.go '* && " $files " != *' watchdog_unix.go '* ]]
}

test_omitting_workflow_command
test_partial_workflow_discovery
test_partial_diagnostic_size
test_partial_go_target_discovery
test_complete_go_target_discovery
test_partial_format_discovery
test_complete_format_execution
test_complete_fuzz_execution
test_partial_tool_resolution
test_aix_refusal_selection
