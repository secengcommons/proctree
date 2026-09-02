#!/usr/bin/env bash
set -euo pipefail

target_os=${1:-}
target_arch=${2:-}
target_cgo=${3:-0}

if [[ ! "$target_os" =~ ^[a-z0-9]+$ || ! "$target_arch" =~ ^[a-z0-9]+$ || ! "$target_cgo" =~ ^[01]$ ]]; then
  printf 'Usage: %s GOOS GOARCH [CGO_ENABLED]\n' "${0##*/}" >&2
  exit 2
fi

temporary=$(mktemp -d "${TMPDIR:-/tmp}/secengcommons-proctree-build-target.XXXXXX")
case "$temporary" in
  "${TMPDIR:-/tmp}"/secengcommons-proctree-build-target.*) ;;
  *) printf 'Unsafe temporary directory: %s\n' "$temporary" >&2; exit 1 ;;
esac
trap 'rm -rf -- "$temporary"' EXIT

package_list=$temporary/packages
env CGO_ENABLED="$target_cgo" GOOS="$target_os" GOARCH="$target_arch" \
  go list -f '{{if or .GoFiles .CgoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... |
  sed '/^[[:space:]]*$/d' >"$package_list"
index=0
while IFS= read -r package; do
  [[ -n "$package" ]] || continue
  env CGO_ENABLED="$target_cgo" GOOS="$target_os" GOARCH="$target_arch" \
    go test -c -trimpath -o "$temporary/$index.test" "$package"
  index=$((index + 1))
done <"$package_list"
if ((index == 0)); then
  printf 'No packages were built for %s/%s\n' "$target_os" "$target_arch" >&2
  exit 1
fi
printf 'Built %s/%s production and test packages\n' "$target_os" "$target_arch"
