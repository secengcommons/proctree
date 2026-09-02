#!/usr/bin/env bash
set -euo pipefail

repository_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
readonly repository_root
mode=${1:-}

if [[ "$mode" != standard && "$mode" != android && "$mode" != ios ]]; then
  printf 'Usage: %s standard|android|ios\n' "${0##*/}" >&2
  exit 2
fi

temporary=$(mktemp -d "${TMPDIR:-/tmp}/secengcommons-proctree-build-targets.XXXXXX")
case "$temporary" in
  "${TMPDIR:-/tmp}"/secengcommons-proctree-build-targets.*) ;;
  *) printf 'Unsafe target-build directory: %s\n' "$temporary" >&2; exit 1 ;;
esac
trap 'rm -rf -- "$temporary"' EXIT
target_inventory=$temporary/targets
if ! go tool dist list >"$target_inventory"; then
  printf 'Go target discovery failed\n' >&2
  exit 1
fi
[[ -s "$target_inventory" ]]
targets=()
while IFS= read -r target; do
  [[ -n "$target" ]] && targets+=("$target")
done <"$target_inventory"
readonly max_go_targets=256
((${#targets[@]} > 0 && ${#targets[@]} <= max_go_targets))
selected_os=()
selected_arch=()

for target in "${targets[@]}"; do
  IFS=/ read -r target_os target_arch trailing <<<"$target"
  [[ "$target_os" =~ ^[a-z0-9]+$ && "$target_arch" =~ ^[a-z0-9]+$ && -z "$trailing" ]]
  if [[ "$mode" == ios && "$target_os" != ios ]] ||
      [[ "$mode" == android && "$target_os" != android ]] ||
      [[ "$mode" == standard && ("$target_os" == android || "$target_os" == ios) ]]; then
    continue
  fi
  selected_os+=("$target_os")
  selected_arch+=("$target_arch")
done
((${#selected_os[@]} > 0))
completed=0

for index in "${!selected_os[@]}"; do
  target_os=${selected_os[index]}
  target_arch=${selected_arch[index]}
  cgo=0
  if [[ "$target_os" == android ]]; then
    case "$target_arch" in
      386) compiler=i686-linux-android21-clang ;;
      amd64) compiler=x86_64-linux-android21-clang ;;
      arm) compiler=armv7a-linux-androideabi21-clang ;;
      arm64) compiler=aarch64-linux-android21-clang ;;
      *) printf 'Unsupported Android architecture: %s\n' "$target_arch" >&2; exit 1 ;;
    esac
    prebuilt=$(find "${ANDROID_NDK_HOME:?}"/toolchains/llvm/prebuilt -mindepth 1 -maxdepth 1 -type d -print -quit)
    [[ -n "$prebuilt" && -x "$prebuilt/bin/$compiler" ]]
    export CC=$prebuilt/bin/$compiler
    cgo=1
  elif [[ "$target_os" == ios ]]; then
    export CC
    CC=$(go env GOROOT)/misc/ios/clangwrap.sh
    [[ -x "$CC" ]]
    cgo=1
  fi
  bash "$repository_root/.github/scripts/build-target.sh" "$target_os" "$target_arch" "$cgo"
  completed=$((completed + 1))
done
((completed == ${#selected_os[@]}))
