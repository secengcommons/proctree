#!/usr/bin/env bash
set -euo pipefail

readonly max_fuzz_targets=1024
inventory_directories=()
inventory_packages=()
inventory_targets=()

discover_fuzz_module() {
  local directory=$1
  local module_found=false
  local packages
  if ! packages=$(go -C "$directory" list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... | sed '/^[[:space:]]*$/d'); then
    printf 'Fuzz package discovery failed in %s\n' "$directory" >&2
    return 1
  fi
  while IFS= read -r package; do
    [[ -n "$package" ]] || continue
    local targets
    if ! targets=$(go -C "$directory" test -list '^Fuzz[A-Za-z0-9_]+$' "$package" | awk '/^Fuzz[A-Za-z0-9_]+$/'); then
      printf 'Fuzz target discovery failed for %s\n' "$package" >&2
      return 1
    fi
    while IFS= read -r target; do
      [[ -n "$target" ]] || continue
      module_found=true
      if ((${#inventory_targets[@]} >= max_fuzz_targets)); then
        printf 'Fuzz target inventory exceeds %d entries\n' "$max_fuzz_targets" >&2
        return 1
      fi
      inventory_directories+=("$directory")
      inventory_packages+=("$package")
      inventory_targets+=("$target")
    done <<<"$targets"
  done <<<"$packages"
  if [[ "$module_found" == false ]]; then
    printf 'No fuzz targets were discovered in %s\n' "$directory" >&2
    exit 1
  fi
}

modules=("$@")
if ((${#modules[@]} == 0)); then
  modules=(. tools/workflow)
fi
for module in "${modules[@]}"; do
  discover_fuzz_module "$module"
done
completed=0
for index in "${!inventory_targets[@]}"; do
  go -C "${inventory_directories[index]}" test -run '^$' \
    -fuzz "^${inventory_targets[index]}$" -fuzztime="${FUZZTIME:-100000x}" "${inventory_packages[index]}"
  completed=$((completed + 1))
done
((completed == ${#inventory_targets[@]}))
