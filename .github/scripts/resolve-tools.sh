#!/usr/bin/env bash
set -euo pipefail

output=${1:-}
if [[ -z "$output" || ! -f "$output" ]]; then
  printf 'Usage: %s OUTPUT\n' "${0##*/}" >&2
  exit 2
fi

if ! golangci=$(go -C tools tool -n golangci-lint); then
  printf 'GolangCI-Lint resolution failed\n' >&2
  exit 1
fi
if ! govulncheck=$(go -C tools tool -n govulncheck); then
  printf 'Govulncheck resolution failed\n' >&2
  exit 1
fi
if ! actionlint=$(go -C tools/workflow tool -n actionlint); then
  printf 'Actionlint resolution failed\n' >&2
  exit 1
fi
if ! shellcheck=$(go -C tools/workflow tool -n shellcheck); then
  printf 'ShellCheck resolution failed\n' >&2
  exit 1
fi
[[ -n "$golangci" && -n "$govulncheck" && -n "$actionlint" && -n "$shellcheck" ]]
{
  printf 'golangci=%s\n' "$golangci"
  printf 'govulncheck=%s\n' "$govulncheck"
  printf 'actionlint=%s\n' "$actionlint"
  printf 'shellcheck=%s\n' "$shellcheck"
} >>"$output"
