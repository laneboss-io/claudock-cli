#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'release: %s\n' "$1" >&2
  exit 1
}

if [[ $# != 2 ]]; then
  printf 'Usage: scripts/release.sh VERSION OUTDIR\n' >&2
  exit 2
fi
release_version=$1
release_out=$2
[[ $release_version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  fail 'VERSION must be a numeric major.minor.patch version.'
[[ -n $release_out ]] || fail 'OUTDIR must not be empty.'

release_script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
release_root=$(git -C "$release_script_dir" rev-parse --show-toplevel)

require_clean() {
  local release_status
  release_status=$(git -C "$release_root" status --porcelain=v1 \
    --untracked-files=all --ignore-submodules=none)
  [[ -z $release_status ]] ||
    fail 'The working tree must be clean, including untracked files.'
}

require_clean
release_commit=$(git -C "$release_root" rev-parse HEAD)
release_code_version=$(git -C "$release_root" show \
  "$release_commit:internal/usage/command.go" | awk '
  /^const Version = "[0-9]+\.[0-9]+\.[0-9]+"$/ {
    value = $4
    gsub(/"/, "", value)
    count++
  }
  END {
    if (count != 1) exit 1
    print value
  }
')
[[ $release_version == "$release_code_version" ]] ||
  fail 'VERSION must match the Version constant in the code.'

for release_tool in go gzip sha256sum; do
  command -v "$release_tool" >/dev/null ||
    fail 'Go, gzip and sha256sum are required.'
done
release_tar=tar
if command -v gtar >/dev/null; then
  release_tar=gtar
fi
[[ $("$release_tar" --version) == *'GNU tar'* ]] ||
  fail 'GNU tar is required for reproducible archives.'

export LC_ALL=C TZ=UTC
umask 022
mkdir -p -- "$release_out"
release_out=$(cd -- "$release_out" && pwd -P)
case "$release_out/" in
  "$release_root/"*)
    git -C "$release_root" check-ignore -q -- \
      "$release_out/.claudock-release" ||
      fail 'OUTDIR inside the checkout must be ignored by Git, such as dist/.'
    ;;
esac

release_stage=$(mktemp -d "${TMPDIR:-/tmp}/claudock-release.XXXXXX")
trap 'rm -rf -- "$release_stage"' EXIT
mkdir -- "$release_stage/source" "$release_stage/package"
# Compile committed bytes, so edits during the build cannot enter an archive.
git -C "$release_root" archive "$release_commit" |
  "$release_tar" -xf - -C "$release_stage/source"
cp -- "$release_stage/source/LICENSE" "$release_stage/package/LICENSE"
cp -- "$release_stage/source/README.md" "$release_stage/package/README.md"
chmod 0644 "$release_stage/package/LICENSE" "$release_stage/package/README.md"

release_assets=()
for release_target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  release_os=${release_target%/*}
  release_arch=${release_target#*/}
  release_asset=claudock_${release_version}_${release_os}_${release_arch}.tar.gz
  release_assets+=("$release_asset")
  (
    cd -- "$release_stage/source"
    CGO_ENABLED=0 GOOS="$release_os" GOARCH="$release_arch" \
      GOAMD64=v1 GOARM64=v8.0 GOFLAGS='' GOEXPERIMENT='' GOWORK=off \
      GOENV=off GOTOOLCHAIN=local \
      go build -mod=readonly -trimpath -buildvcs=false \
      -ldflags='-s -w -buildid=' \
      -o "$release_stage/package/claudock" ./cmd/claudock
  )
  chmod 0755 "$release_stage/package/claudock"
  "$release_tar" --format=ustar --sort=name --mtime='1970-01-01 00:00:00Z' \
    --owner=0 --group=0 --numeric-owner \
    -C "$release_stage/package" -cf - LICENSE README.md claudock |
    gzip -n -9 > "$release_stage/$release_asset"
done
(
  cd -- "$release_stage"
  sha256sum -- "${release_assets[@]}" > SHA256SUMS
)

require_clean
[[ $(git -C "$release_root" rev-parse HEAD) == "$release_commit" ]] ||
  fail 'HEAD changed during the build.'
for release_asset in "${release_assets[@]}" SHA256SUMS; do
  [[ ! -e "$release_out/$release_asset" && ! -L "$release_out/$release_asset" ]] ||
    fail 'OUTDIR already contains release assets; use a fresh directory.'
done
# Publish the manifest last; a failed build never publishes a checksum list.
for release_asset in "${release_assets[@]}" SHA256SUMS; do
  mv -- "$release_stage/$release_asset" "$release_out/$release_asset"
done
printf 'Created four release archives and SHA256SUMS.\n'
