#!/usr/bin/env bash
# Publishes the generated Swift package as one commit on the swiftpm branch and tags it vX.Y.Z.
# GitHub signs the commit: createCommitOnBranch with this repository's GITHUB_TOKEN. No signing
# key secret is needed. The owner seeds the swiftpm branch with one signed commit before the
# first release.
set -euo pipefail
[[ $# == 3 ]] || { echo 'usage: publish-swift-package.sh <version> <package-directory> <source-sha>' >&2; exit 125; }
version=$1
package=$(cd "$2" && pwd)
source_sha=$3
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ && "$source_sha" =~ ^[0-9a-f]{40}$ ]] || { echo 'invalid version or source commit' >&2; exit 125; }
: "${GITHUB_REPOSITORY:?repository is required}"
: "${GH_TOKEN:?repository token is required}"
repo=$GITHUB_REPOSITORY
for file in Package.swift Sources/CCSProjectImage/embedded_image.c Sources/CCSProjectImage/cs-project-image.tar.gz; do
    [[ -f "$package/$file" ]] || { echo "incomplete package: $file" >&2; exit 1; }
done
temporary=$(mktemp -d)
cleanup() { find "$temporary" -depth -delete; }
trap cleanup EXIT
source_line="Source: release/$version ($source_sha)"

# "path blob-sha" of each package file, sorted.
(cd "$package" && find . -type f | sed 's|^\./||' | LC_ALL=C sort | while read -r file; do
    printf '%s %s\n' "$file" "$(git hash-object "$file")"
done) > "$temporary/local.txt"
remote_blobs() { # commit
    gh api "repos/$repo/git/trees/$1?recursive=1" > "$temporary/tree.json"
    jq -e '.truncated == false' "$temporary/tree.json" >/dev/null
    jq -r '.tree[] | select(.type == "blob") | "\(.path) \(.sha)"' "$temporary/tree.json" | LC_ALL=C sort
}

# Only a real HTTP 404 means the tag is absent. Auth, rate-limit, network and server errors must
# not be mistaken for permission to create another commit.
if gh api --include "repos/$repo/git/ref/tags/v$version" > "$temporary/tag-response" 2> "$temporary/tag-error"; then
    sed '1,/^[[:space:]]*$/d' "$temporary/tag-response" > "$temporary/tag.json"
    jq -e '.object.type == "commit"' "$temporary/tag.json" >/dev/null
    existing=$(jq -er '.object.sha' "$temporary/tag.json")
    gh api "repos/$repo/commits/$existing" > "$temporary/commit.json"
    jq -e --arg source "$source_line" '.commit.verification.verified == true and (.commit.message | split("\n") | index($source) != null)' \
        "$temporary/commit.json" >/dev/null || { echo "v$version is not a signed commit of $source_line" >&2; exit 1; }
    remote_blobs "$existing" > "$temporary/remote.txt"
    cmp -s "$temporary/local.txt" "$temporary/remote.txt" || { echo "v$version has different package files" >&2; exit 1; }
    printf 'Verified existing v%s at %s\n' "$version" "$existing"
    exit 0
fi
status=$(head -n 1 "$temporary/tag-response" | awk '{print $2}')
[[ "$status" == 404 ]] || { echo "tag lookup failed (HTTP ${status:-unknown})" >&2; exit 1; }

head=$(gh api "repos/$repo/git/ref/heads/swiftpm" | jq -er '.object.sha')
# base64 first: jq reads files as UTF-8 and would change the archive bytes.
while read -r file _; do
    base64 < "$package/$file" | tr -d '\n' > "$temporary/contents"
    jq -n --arg path "$file" --rawfile contents "$temporary/contents" '{path:$path,contents:$contents}'
done < "$temporary/local.txt" | jq -s . > "$temporary/additions.json"
remote_blobs "$head" | cut -d ' ' -f1 > "$temporary/head-paths.txt"
cut -d ' ' -f1 "$temporary/local.txt" > "$temporary/local-paths.txt"
LC_ALL=C comm -23 "$temporary/head-paths.txt" "$temporary/local-paths.txt" | jq -R '{path:.}' | jq -s . > "$temporary/deletions.json"
jq -n --arg repo "$repo" --arg head "$head" --arg version "$version" --arg source "$source_line" \
    --slurpfile additions "$temporary/additions.json" --slurpfile deletions "$temporary/deletions.json" \
    '{query:"mutation($input:CreateCommitOnBranchInput!){createCommitOnBranch(input:$input){commit{oid}}}",variables:{input:{branch:{repositoryNameWithOwner:$repo,branchName:"swiftpm"},expectedHeadOid:$head,message:{headline:("release: cs-project v"+$version+" swift package"),body:$source},fileChanges:{additions:$additions[0],deletions:$deletions[0]}}}}' \
    > "$temporary/request.json"
gh api graphql --input "$temporary/request.json" > "$temporary/response.json"
commit=$(jq -er '.data.createCommitOnBranch.commit.oid' "$temporary/response.json")
gh api "repos/$repo/commits/$commit" | jq -e '.commit.verification.verified == true' >/dev/null \
    || { echo "package commit $commit is not signed" >&2; exit 1; }
gh api --method POST "repos/$repo/git/refs" -f "ref=refs/tags/v$version" -f "sha=$commit" >/dev/null
printf 'Published signed package commit %s as v%s\n' "$commit" "$version"
