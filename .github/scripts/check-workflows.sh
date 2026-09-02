#!/usr/bin/env bash
set -euo pipefail

if [[ $- == *x* ]]; then
  exec {xtrace_fd}>&2
  BASH_XTRACEFD=$xtrace_fd
fi

repository_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
temporary=$(mktemp -d "${TMPDIR:-/tmp}/secengcommons-proctree-check-workflows.XXXXXX")
case "$temporary" in
  "${TMPDIR:-/tmp}"/secengcommons-proctree-check-workflows.*) ;;
  *) printf 'Unsafe workflow-check directory: %s\n' "$temporary" >&2; exit 1 ;;
esac
trap 'rm -rf -- "$temporary"' EXIT

candidates=$temporary/workflow-candidates
if ! find "$repository_root/.github/workflows" -maxdepth 1 -type f \( -name '*.yml' -o -name '*.yaml' \) -print >"$candidates"; then
  printf 'Workflow discovery failed\n' >&2
  exit 1
fi
ordered=$temporary/workflows
LC_ALL=C sort "$candidates" >"$ordered"
mapfile -t workflows <"$ordered"
((${#workflows[@]} > 0 && ${#workflows[@]} <= 32))

checker=$temporary/workflowpolicy
build=(-trimpath -o "$checker")
coverage_directory=${WORKFLOW_POLICY_COVERAGE_DIR:-}
if [[ -n "$coverage_directory" ]]; then
  [[ -d "$coverage_directory" ]]
  build+=(-cover -covermode=atomic -coverpkg=./cmd/workflowpolicy)
fi
go -C "$repository_root/tools/workflow" build "${build[@]}" ./cmd/workflowpolicy

run_checker() {
  if [[ -n "$coverage_directory" ]]; then
    GOCOVERDIR=$coverage_directory "$checker" "$@"
  else
    "$checker" "$@"
  fi
}

invalid=$temporary/invalid.yml
cat >"$invalid" <<'EOF'
name: Invalid
on: push
jobs:
  policy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@main
EOF
readonly max_diagnostic_bytes=$((4 * 1024))
fixtures=$temporary/fixtures
mkdir -- "$fixtures"
fixture_workflows=()
for workflow in "${workflows[@]}"; do
  fixture=$fixtures/${workflow##*/}
  cp -- "$workflow" "$fixture"
  fixture_workflows+=("$fixture")
done

expect_rejection() {
  local label=$1
  local expected=$2
  shift 2
  local stderr=$temporary/$label.stderr
  local stdout=$temporary/$label.stdout
  set +e
  run_checker "$@" >"$stdout" 2>"$stderr"
  local status=$?
  set -e
  [[ "$status" -eq 1 && ! -s "$stdout" && -s "$stderr" ]]
  local size
  if ! size=$(wc -c <"$stderr"); then
    printf 'Diagnostic size inspection failed\n' >&2
    return 1
  fi
  [[ "$size" =~ ^[0-9]+$ ]]
  ((size <= max_diagnostic_bytes))
  grep -Fq "$expected" "$stderr"
  grep -Fq 'workflow action is not pinned to a full commit' "$stderr"
}

diagnostic_path() {
  local path=$1
  if [[ "${OSTYPE:-}" == msys* || "${OSTYPE:-}" == cygwin* ]]; then
    local native
    if ! native=$(cygpath -m -- "$path"); then
      printf 'Diagnostic path conversion failed\n' >&2
      return 1
    fi
    [[ -n "$native" && ${#native} -le 4096 && "$native" != *$'\n'* ]]
    printf '%s\n' "$native"
    return
  fi
  printf '%s\n' "$path"
}

run_checker "${workflows[@]}"
for index in "${!fixture_workflows[@]}"; do
  mutated=${fixture_workflows[index]}
  cp -- "$invalid" "$mutated"
  expected=$(diagnostic_path "$mutated")
  expect_rejection "invalid-$index" "$expected" "${fixture_workflows[@]}"
  cp -- "${workflows[index]}" "$mutated"
done
