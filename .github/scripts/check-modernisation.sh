#!/usr/bin/env bash
set -euo pipefail

linter=${1:-}
repository_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
temporary=$(mktemp -d "${TMPDIR:-/tmp}/secengcommons-proctree-modernisation.XXXXXX")
case "$temporary" in
  "${TMPDIR:-/tmp}"/secengcommons-proctree-modernisation.*) ;;
  *) printf 'Unsafe temporary directory: %s\n' "$temporary" >&2; exit 1 ;;
esac
trap 'rm -rf -- "$temporary"' EXIT
[[ -x "$linter" ]]

cat >"$temporary/go.mod" <<'EOF'
module modernisation.test/probe

go 1.26.0
EOF
cat >"$temporary/probe.go" <<'EOF'
package probe

func boundedIndex(index int, values []int) int {
	maximum := index
	if len(values) < index {
		maximum = len(values)
	}
	return maximum
}
EOF

fix=$temporary/fix.out
if (cd -- "$temporary" && go fix -diff ./...) >"$fix" 2>&1; then
  printf 'Go fix accepted legacy source\n' >&2
  exit 1
fi
grep -Fq 'min(len(values), index)' "$fix"
lint=$temporary/lint.out
if (cd -- "$temporary" && "$linter" run --config "$repository_root/.golangci.yml" ./...) >"$lint" 2>&1; then
  printf 'Modernisation analyser accepted legacy source\n' >&2
  exit 1
fi
grep -Fq '(modernize)' "$lint"
