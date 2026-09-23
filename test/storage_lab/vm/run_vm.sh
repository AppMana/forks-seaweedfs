#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
labcontainers_source=${LABCONTAINERS_SOURCE:-"$(dirname -- "$repo_root")/labcontainers"}

case "$labcontainers_source" in
  /*) ;;
  *) echo "LABCONTAINERS_SOURCE must be an absolute path" >&2; exit 2 ;;
esac
test -f "$labcontainers_source/go.mod" || {
  echo "Labcontainers source not found at $labcontainers_source" >&2
  exit 2
}

: "${LABCONTAINERS_CONTAINERLAB:?Set an absolute path to the SDK-qualified Containerlab CLI (including its native recovery patches)}"
case "$LABCONTAINERS_CONTAINERLAB" in
  /*) test -x "$LABCONTAINERS_CONTAINERLAB" ;;
  *) echo "LABCONTAINERS_CONTAINERLAB must be an absolute executable path" >&2; exit 2 ;;
esac
export LABCONTAINERS_CONTAINERLAB

if [ -n "${LABCONTAINERS_REF:-}" ]; then
  case "$LABCONTAINERS_REF" in
    *[!0-9a-fA-F]*|'') echo "LABCONTAINERS_REF must be a full 40-character commit SHA" >&2; exit 2 ;;
  esac
  test "${#LABCONTAINERS_REF}" -eq 40 || {
    echo "LABCONTAINERS_REF must be a full 40-character commit SHA" >&2
    exit 2
  }
  actual=$(git -C "$labcontainers_source" rev-parse HEAD)
  test "$actual" = "$LABCONTAINERS_REF" || {
    echo "Labcontainers checkout $actual does not match LABCONTAINERS_REF $LABCONTAINERS_REF" >&2
    exit 2
  }
fi

sdk_pin=$(sed -n 's/.*github.com\/appmana\/labcontainers .*-[^-]*-\([0-9a-f]*\)$/\1/p' "$repo_root/test/storage_lab/vm/go.mod")
actual=$(git -C "$labcontainers_source" rev-parse HEAD)
test -n "$sdk_pin" && test "$(printf '%.12s' "$actual")" = "$sdk_pin" || {
  echo "Labcontainers checkout must match the SDK revision pinned in vm/go.mod" >&2
  exit 2
}
test -z "$(git -C "$labcontainers_source" status --porcelain --untracked-files=no)" || {
  echo "Labcontainers tracked sources must be clean for qualification" >&2
  exit 2
}
: "${LABCONTAINERS_VM_IMAGE:?Set the image built with the pinned guest helper}"
test "$(docker image inspect --format '{{ index .Config.Labels "appmana.labcontainers.revision" }}' "$LABCONTAINERS_VM_IMAGE")" = "$actual" || {
  echo "VM guest helper revision does not match the SDK and daemon source" >&2
  exit 2
}

tmp=$(mktemp -d /tmp/seaweedfs-labcontainers.XXXXXXXX)
trap 'rm -rf -- "$tmp"' EXIT HUP INT TERM

(cd "$labcontainers_source" && go build -o "$tmp/labd" ./cmd/labd)
(cd "$tmp" && go work init "$repo_root/test/storage_lab/vm" "$labcontainers_source")
GOWORK="$tmp/go.work" go run "$repo_root/test/storage_lab/vm" --labd "$tmp/labd" "$@"
