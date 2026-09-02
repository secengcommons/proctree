#!/usr/bin/env bash
set -euo pipefail

repository_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
readonly repository_root
readonly alpine_image='alpine@sha256:79ff19e9084a00eece421b2523fb93e22d730e2c0e525905de047e848e56d95f'
temporary=$(mktemp -d "${TMPDIR:-/tmp}/secengcommons-proctree-test-container.XXXXXX")
case "$temporary" in
  "${TMPDIR:-/tmp}"/secengcommons-proctree-test-container.*) ;;
  *) printf 'Unsafe container directory: %s\n' "$temporary" >&2; exit 1 ;;
esac
cleanup() {
  if [[ -d "$temporary" ]]; then
    chmod 0700 "$temporary"
    rm -rf -- "$temporary"
  fi
}
trap cleanup EXIT

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C "$repository_root" test -c -trimpath -o "$temporary/proctree.test" .
chmod 0555 "$temporary/proctree.test"
chmod 0555 "$temporary"
docker image pull "$alpine_image"
common=(
  --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges
  --pids-limit 256 --user 65532:65532 --tmpfs '/tmp:rw,nosuid,nodev,mode=1777,size=64m'
  --mount "type=bind,src=$temporary,dst=/tests,readonly"
  --env HOME=/tmp --env TEMP=/tmp --env TMP=/tmp
)
tests='TestRunBasicOutcomes|TestUnixOwnerNativeTrees|TestUnixOwnerDeadlineTerminatesTree|TestUnixWatchdogKillsTreeWhenControllerExits|TestLinuxEscapedWriterDoesNotExtendCleanup|TestLinuxEscapedWriterDiesWithTestRunner|TestLinuxEscapedWriterDiesWithOuterRunner'
docker run "${common[@]}" --env PROCTREE_PID1_QUALIFICATION=1 --entrypoint /tests/proctree.test "$alpine_image" -test.run '^TestLinuxPID1ReportsCleanupFailure$' -test.count=1
docker run --init "${common[@]}" --entrypoint /bin/sh "$alpine_image" -c "/tests/proctree.test -test.run '^($tests)$' -test.count=1"
