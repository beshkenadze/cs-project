#!/usr/bin/env bash
# Runs scripts/publish-swift-package.sh against a mock gh. No network access.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
temporary=$(mktemp -d)
trap 'find "$temporary" -depth -delete' EXIT
package=$temporary/package
sources=$package/Sources/CCSProjectImage
mkdir -p "$temporary/bin" "$sources"
printf 'package\n' > "$package/Package.swift"
printf 'embed\n' > "$sources/embedded_image.c"
# Bytes that are not UTF-8, as in a gzip archive.
printf '\x1f\x8b\x08\x00\xff\xfe\x80\x00\xc3\x28' > "$sources/cs-project-image.tar.gz"

export TEST_TREE=$temporary/tree.json TEST_REQUEST=$temporary/request.json
(cd "$package" && find . -type f | sed 's|^\./||' | while read -r f; do
    jq -n --arg path "$f" --arg sha "$(git hash-object "$f")" '{path:$path,type:"blob",sha:$sha}'
done) | jq -s '{truncated:false,tree:(. + [{path:"Sources",type:"tree",sha:"t"},{path:"old.txt",type:"blob",sha:"o"}])}' > "$TEST_TREE"
export TEST_MARKER=$temporary/mutated GH_TOKEN=test GITHUB_REPOSITORY=test/cs-project
export TEST_SOURCE=0123456789012345678901234567890123456789
cat > "$temporary/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
call="$*"
case "$call" in
  *git/ref/tags/*)
    case "$TEST_CASE" in
      server-error) printf 'HTTP/2.0 500 Internal Server Error\r\n\r\n{}\n'; exit 1 ;;
      new) printf 'HTTP/2.0 404 Not Found\r\n\r\n{}\n'; exit 1 ;;
    esac
    printf 'HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n{"object":{"type":"commit","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}\n'
    ;;
  *git/ref/heads/swiftpm*) printf '{"object":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}\n' ;;
  *'/commits/'*)
    verified=true
    source=$TEST_SOURCE
    [[ "$TEST_CASE" != unsigned ]] || verified=false
    [[ "$TEST_CASE" != wrong-source ]] || source=ffffffffffffffffffffffffffffffffffffffff
    jq -n --arg source "$source" --argjson verified "$verified" '{commit:{verification:{verified:$verified},message:("release: cs-project v0.1.0 swift package\n\nSource: release/0.1.0 ("+$source+")")}}'
    ;;
  *'/git/trees/'*)
    if [[ "$TEST_CASE" == wrong-bytes ]]; then
        jq '(.tree[] | select(.path == "Sources/CCSProjectImage/cs-project-image.tar.gz") | .sha) = "different"' "$TEST_TREE"
    elif [[ "$TEST_CASE" == new ]]; then
        cat "$TEST_TREE"
    else
        jq 'del(.tree[] | select(.path == "old.txt"))' "$TEST_TREE"
    fi
    ;;
  'api graphql --input '*)
    [[ "$TEST_CASE" == new ]] || { touch "$TEST_MARKER"; exit 1; }
    cp "$4" "$TEST_REQUEST"
    printf '{"data":{"createCommitOnBranch":{"commit":{"oid":"cccccccccccccccccccccccccccccccccccccccc"}}}}\n'
    ;;
  *'--method POST'*'git/refs'*)
    [[ "$TEST_CASE" == new ]] || { touch "$TEST_MARKER"; exit 1; }
    ;;
  *) touch "$TEST_MARKER"; echo "unexpected API call: $call" >&2; exit 1 ;;
esac
MOCK
chmod +x "$temporary/bin/gh"
export PATH="$temporary/bin:$PATH"
publisher=$root/scripts/publish-swift-package.sh
fail() { cat "$temporary/output"; echo "FAIL $*" >&2; exit 1; }
for scenario in new retry server-error unsigned wrong-source wrong-bytes; do
    export TEST_CASE=$scenario
    if "$publisher" 0.1.0 "$package" "$TEST_SOURCE" > "$temporary/output" 2>&1; then
        [[ "$scenario" == new || "$scenario" == retry ]] || fail "unexpected success: $scenario"
    else
        [[ "$scenario" != new && "$scenario" != retry ]] || fail "unexpected failure: $scenario"
    fi
    [[ ! -e "$TEST_MARKER" ]] || fail "unexpected mutation: $scenario"
    printf 'PASS publication %s\n' "$scenario"
done

input=$(jq -c '.variables.input' "$TEST_REQUEST")
jq -r '.fileChanges.additions[] | select(.path == "Sources/CCSProjectImage/cs-project-image.tar.gz") | .contents' <<< "$input" \
    | base64 -d > "$temporary/decoded"
cmp -s "$temporary/decoded" "$sources/cs-project-image.tar.gz" || fail 'archive bytes changed in the commit request'
[[ $(jq -c '[.fileChanges.additions[].path] | sort' <<< "$input") == '["Package.swift","Sources/CCSProjectImage/cs-project-image.tar.gz","Sources/CCSProjectImage/embedded_image.c"]' ]] \
    || fail 'wrong additions'
[[ $(jq -c '.fileChanges.deletions' <<< "$input") == '[{"path":"old.txt"}]' ]] || fail 'wrong deletions'
jq -e '.branch.branchName == "swiftpm" and .expectedHeadOid == "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' <<< "$input" >/dev/null || fail 'wrong branch'
jq -e '.message.body | startswith("Source: release/0.1.0 (0123456789012345678901234567890123456789)")' <<< "$input" >/dev/null || fail 'wrong source line'
echo 'PASS publication request'
