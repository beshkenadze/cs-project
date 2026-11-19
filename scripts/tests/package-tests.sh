#!/usr/bin/env bash
# Checks the Swift package that build.sh generates in stage/swift. Run it after build.sh.
# The release publishes the package with one createCommitOnBranch request. GitHub limits that
# request to about 45 MB, and the request carries the files in base64.
set -euo pipefail
cd "$(dirname "$0")/../.."
archive=stage/cs-project-image.tar.gz
pkg=stage/swift
sources=$pkg/Sources/CCSProjectImage
fail() { echo "FAIL $*" >&2; exit 1; }

[[ -f "$archive" && -f "$pkg/Package.swift" ]] || fail 'run build.sh first'
sha=$(shasum -a 256 "$archive" | cut -d ' ' -f1)

cmp -s "$archive" "$sources/cs-project-image.tar.gz" || fail 'package archive differs from the build archive'
grep -q "return \"$sha\";" "$sources/embedded_image.c" || fail 'package SHA-256 differs from the archive SHA-256'
grep -q '^#embed "cs-project-image.tar.gz"$' "$sources/embedded_image.c" || fail 'package does not embed the archive file'

limit=$((30 * 1024 * 1024))
total=$(find "$pkg" -type f -exec cat {} + | wc -c | tr -d '[:space:]')
[[ "$total" -le "$limit" ]] || fail "package has $total bytes, limit $limit"
echo "PASS package $total bytes, archive SHA-256 $sha"
