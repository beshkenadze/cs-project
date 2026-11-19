#!/usr/bin/env bash
# Builds the launcher, checks the pinned runc and CNI releases, makes the launcher image and
# generates the Swift package in stage/swift. Runs on macOS: Go cross-compiles for linux/arm64.
set -euo pipefail
cd "$(dirname "$0")"

version=0.1.0-dev
reference="cs-project:$version"
runc_url=https://github.com/opencontainers/runc/releases/download/v1.5.3/runc.arm64
runc_sha=07b52aee923cc432c38fe4fb6809a08fc6471994efed650af3635350588a20be
cni_url=https://github.com/containernetworking/plugins/releases/download/v1.10.0/cni-plugins-linux-arm64-v1.10.0.tgz
cni_sha=4f25d7f136be368746185fba6557d799cf3e1890f7556d450744ea20d300d68c

out=stage
cache="$out/cache"
mkdir -p "$cache" "$out/bin"
fetch() { # url sha file
    if [[ ! -f "$3" ]] || ! echo "$2  $3" | shasum -a 256 -c - >/dev/null 2>&1; then
        curl -fLsS --max-time 120 -o "$3.part" "$1"
        mv "$3.part" "$3"
    fi
    echo "$2  $3" | shasum -a 256 -c - >/dev/null || { echo "SHA-256 mismatch: $3" >&2; exit 1; }
}
fetch "$runc_url" "$runc_sha" "$cache/runc.arm64"
fetch "$cni_url" "$cni_sha" "$cache/cni.tgz"
tar -xzf "$cache/cni.tgz" -C "$out/bin" ./bridge ./loopback ./static

go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '-s -w' -o "$out/bin/cs-project" ./cmd/cs-project
archive="$out/cs-project-image.tar.gz"
go run ./cmd/mkimage -ref "$reference" -out "$archive" \
    /cs-project="$out/bin/cs-project" /bin/runc="$cache/runc.arm64" \
    /opt/cni/bin/bridge="$out/bin/bridge" /opt/cni/bin/loopback="$out/bin/loopback" /opt/cni/bin/static="$out/bin/static"

sha=$(shasum -a 256 "$archive" | cut -d ' ' -f1)
pkg="$out/swift"
rm -rf "$pkg"
cp -R packaging/swift "$pkg"
{
    xxd -i -n cs_project_image_data "$archive" \
        | sed -E 's/^unsigned char /static const unsigned char /; s/^unsigned int /static const unsigned int /'
    printf '\n#include "CCSProjectImage.h"\n'
    printf 'const unsigned char *cs_project_image_bytes(void) { return cs_project_image_data; }\n'
    printf 'unsigned int cs_project_image_len(void) { return cs_project_image_data_len; }\n'
    printf 'const char *cs_project_image_sha256(void) { return "%s"; }\n' "$sha"
    printf 'const char *cs_project_image_reference(void) { return "%s"; }\n' "$reference"
} > "$pkg/Sources/CCSProjectImage/embedded_image.c"
echo "image $reference archive SHA-256 $sha"
