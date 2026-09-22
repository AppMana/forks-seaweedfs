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

tmp=$(mktemp -d /tmp/seaweedfs-labcontainers.XXXXXXXX)
trap 'rm -rf -- "$tmp"' EXIT HUP INT TERM

(cd "$labcontainers_source" && go build -o "$tmp/labd" ./cmd/labd)
(cd "$tmp" && go work init "$repo_root/test/storage_lab/vm" "$labcontainers_source")
GOWORK="$tmp/go.work" go run "$repo_root/test/storage_lab/vm" --labd "$tmp/labd" "$@"
