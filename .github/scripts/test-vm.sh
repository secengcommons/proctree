#!/usr/bin/env bash
set -euo pipefail

repository_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
readonly repository_root
manifest=$repository_root/.tests/manifest.tsv
count=0

[[ -f "$manifest" && ! -L "$manifest" ]] || {
  printf 'VM test manifest is unavailable\n' >&2
  exit 1
}
while IFS=$'\t' read -r directory binary; do
  count=$((count + 1))
  if ((count > 128)) || [[ ! "$directory" =~ ^[A-Za-z0-9_./-]+$ ]] ||
      [[ "$directory" == .. || "$directory" == ../* || "$directory" == */../* || "$directory" == */.. ]] ||
      [[ ! "$binary" =~ ^\.tests/[0-9]+\.test$ ]]; then
    printf 'Invalid VM test manifest entry\n' >&2
    exit 1
  fi
  executable=$repository_root/$binary
  [[ -f "$executable" && ! -L "$executable" ]] || {
    printf 'VM test binary is unavailable: %s\n' "$binary" >&2
    exit 1
  }
  chmod 0700 "$executable"
  (cd -- "$repository_root/$directory" && "$executable" -test.count=1 -test.shuffle=on -test.timeout=10m)
done <"$manifest"
if ((count == 0)); then
  printf 'VM test manifest is empty\n' >&2
  exit 1
fi
printf 'Tested %d VM binaries\n' "$count"
