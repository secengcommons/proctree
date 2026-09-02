#!/usr/bin/env bash
set -euo pipefail

linter=${1:-}
if [[ -z "$linter" || ! -x "$linter" ]]; then
  printf 'Usage: %s GOLANGCI_LINT\n' "${0##*/}" >&2
  exit 2
fi

temporary=$(mktemp -d "${TMPDIR:-/tmp}/secengcommons-proctree-format.XXXXXX")
case "$temporary" in
  "${TMPDIR:-/tmp}"/secengcommons-proctree-format.*) ;;
  *) printf 'Unsafe format directory: %s\n' "$temporary" >&2; exit 1 ;;
esac
trap 'rm -rf -- "$temporary"' EXIT
inventory=$temporary/go-files
if ! find tools/workflow -type f -name '*.go' -print0 >"$inventory"; then
  printf 'Workflow Go-file discovery failed\n' >&2
  exit 1
fi
[[ -s "$inventory" ]]
mapfile -d '' -t go_files <"$inventory"
readonly max_go_files=1024
((${#go_files[@]} > 0 && ${#go_files[@]} <= max_go_files))
if ! difference=$(gofmt -l "${go_files[@]}"); then
  printf 'Workflow formatting failed\n' >&2
  exit 1
fi
[[ -z "$difference" ]] || { printf '%s\n' "$difference"; exit 1; }
"$linter" fmt --diff
