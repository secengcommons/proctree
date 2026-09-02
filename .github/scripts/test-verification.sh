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

test_partial_go_target_discovery
test_complete_go_target_discovery
test_complete_fuzz_execution
test_partial_tool_resolution
test_aix_refusal_selection
