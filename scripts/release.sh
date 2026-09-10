#!/usr/bin/env bash
# Release the three modules of the repository in lockstep under one version.
#
#   scripts/release.sh v1.3.0            # commit, tag, verify, push the commit and the three tags
#   scripts/release.sh v1.3.0 --no-push  # the same, stopping before the push
#
# On a clean tree at the commit to release it
#   1. checks CHANGELOG.md has a `## [X.Y.Z]` section (the release notes come from it);
#   2. sets the inter-module requires to the version (mysql/go.mod requires the root,
#      cmd/sqlshape/go.mod requires the root and mysql), commits, and tags the commit
#      vX.Y.Z, mysql/vX.Y.Z and cmd/sqlshape/vX.Y.Z;
#   3. fills the go.sum of mysql/ and cmd/sqlshape with the sums of those versions. The tags
#      exist only here so far, so it points `go` at this clone (GOPRIVATE makes it fetch
#      the module through git, and git is told to read it from here) in a fresh module
#      cache; module zips hash by content, so the sums match what the public proxy computes
#      once the tags are pushed. The commit is amended and the tags moved onto it, and the
#      tidy is repeated against the moved tags until nothing changes (tidy may rewrite
#      mysql/go.mod, whose hash cmd/sqlshape's go.sum records);
#   4. builds cmd/sqlshape the way `go install …/cmd/sqlshape@vX.Y.Z` will (outside the
#      workspace, from the tagged modules), and runs the smoke test on the result;
#   5. pushes the commit and the three tags. The root tag triggers .github/workflows/release.yml.
#
# Between step 2 and the push the workspace does not build: every `go` command reads the
# required versions' go.mod from the proxy, and they are not there yet. A failure before
# the push rolls the commit and tags back. Do not ask the public proxy about the version
# before the push (not even a curl of its .info): a miss is cached for a while and
# `go install` and the release workflow then fail on "unknown revision".
set -euo pipefail
cd "$(dirname "$0")/.."
root=$(pwd)

version=${1:-}
push=1
case "${2:-}" in
  "") ;;
  --no-push) push=0 ;;
  *) echo "usage: scripts/release.sh vX.Y.Z [--no-push]" >&2; exit 2 ;;
esac
case "$version" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "usage: scripts/release.sh vX.Y.Z [--no-push]" >&2; exit 2 ;;
esac
if [ -n "$(git status --porcelain)" ]; then
  echo "release: the working tree is not clean" >&2; exit 1
fi
tags=("$version" "mysql/$version" "cmd/sqlshape/$version")
for t in "${tags[@]}"; do
  if git rev-parse -q --verify "refs/tags/$t" >/dev/null; then
    echo "release: tag $t already exists" >&2; exit 1
  fi
done
bare=${version#v}; bare=${bare%%-*}
if ! grep -q "^## \[$bare\]" CHANGELOG.md; then
  echo "release: CHANGELOG.md has no section '## [$bare]'" >&2; exit 1
fi

base=$(git rev-parse HEAD)
rollback() {
  echo "release: rolling back to $base" >&2
  for t in "${tags[@]}"; do git tag -d "$t" >/dev/null 2>&1 || true; done
  git reset -q --hard "$base"
  git clean -q -f -- mysql/go.sum cmd/sqlshape/go.sum
}
cache=$(mktemp -d)
cleanup() {
  GOMODCACHE="$cache" go clean -modcache
  rm -rf "$cache"
}
trap 'rollback' ERR

# 2. requires, commit, tags
(cd mysql && go mod edit -require="github.com/kr9ly/sqlshape@$version")
(cd cmd/sqlshape && go mod edit -require="github.com/kr9ly/sqlshape@$version" -require="github.com/kr9ly/sqlshape/mysql@$version")
git add mysql/go.mod cmd/sqlshape/go.mod
git commit -q -m "release: $version"
for t in "${tags[@]}"; do git tag -a "$t" -m "sqlshape $version"; done

# 3. go.sum from this clone: `go` fetches github.com/kr9ly/sqlshape through git, and git is
# told to read it from here instead
trap 'rollback; cleanup' ERR
export GOWORK=off GOPRIVATE=github.com/kr9ly/sqlshape GOMODCACHE="$cache"
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="url.$root.insteadOf" GIT_CONFIG_VALUE_0=https://github.com/kr9ly/sqlshape
# Repeated until nothing changes: tidy may rewrite mysql/go.mod itself (a dependency that
# became direct), and cmd/sqlshape's go.sum records the hash of mysql/go.mod at the tag, so
# after an amend the tag has to move and cmd/sqlshape has to be tidied again against it.
# The root zip contains neither nested module, so its sums hold across the amends.
for _ in 1 2 3 4; do
  (cd mysql && go mod tidy)
  (cd cmd/sqlshape && go mod tidy)
  if [ -z "$(git status --porcelain)" ]; then break; fi
  git add mysql/go.sum cmd/sqlshape/go.sum mysql/go.mod cmd/sqlshape/go.mod
  git commit -q --amend --no-edit
  for t in "${tags[@]}"; do git tag -f -a "$t" -m "sqlshape $version" >/dev/null; done
  # the tags moved: what was fetched from them is stale, and so are the sums recorded for them
  GOMODCACHE="$cache" go clean -modcache
  sed -i '/^github.com\/kr9ly\/sqlshape/d' mysql/go.sum cmd/sqlshape/go.sum
done
if [ -n "$(git status --porcelain)" ]; then
  echo "release: go.mod / go.sum did not settle" >&2; false
fi

# 4. the binary as `go install` will build it, and the smoke test over the examples
bindir=$(mktemp -d)
bin=$bindir/sqlshape
(cd cmd/sqlshape && CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/kr9ly/sqlshape/internal/cli.version=$version" -o "$bin" .)
# the smoke test runs `go vet` in the workspace, which reads the same not-yet-public versions
GOWORK= .github/scripts/smoke.sh "$bin" "$version"
rm -rf "$bindir"
unset GOWORK GOPRIVATE GOMODCACHE GIT_CONFIG_COUNT GIT_CONFIG_KEY_0 GIT_CONFIG_VALUE_0
cleanup
trap 'rollback' ERR

echo "release: $version at $(git rev-parse --short HEAD), tags ${tags[*]}"
if [ "$push" = 1 ]; then
  git push origin HEAD "${tags[@]}"
else
  echo "release: not pushed; the workspace builds again once these are upstream:"
  echo "  git push origin HEAD ${tags[*]}"
  echo "or undo with: git tag -d ${tags[*]} && git reset --hard $base"
fi
