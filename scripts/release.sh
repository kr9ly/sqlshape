#!/usr/bin/env bash
# Release the three modules of the repository in lockstep under one version.
#
#   scripts/release.sh v1.3.0          # set requires, commit, tag (nothing leaves the machine)
#   scripts/release.sh v1.3.0 --push   # ... and push the commit and the three tags
#
# What it does, on a clean tree at the commit to release:
#   1. checks CHANGELOG.md has a `## [X.Y.Z]` section (the release notes come from it)
#   2. sets the inter-module requires to the version: mysql/go.mod requires the root,
#      cmd/sqlshape/go.mod requires the root and mysql
#   3. commits, and tags that commit vX.Y.Z, mysql/vX.Y.Z and cmd/sqlshape/vX.Y.Z
# The root tag is what triggers .github/workflows/release.yml.
set -euo pipefail
cd "$(dirname "$0")/.."

version=${1:-}
push=${2:-}
case "$version" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "usage: scripts/release.sh vX.Y.Z [--push]" >&2; exit 2 ;;
esac
if [ -n "$push" ] && [ "$push" != --push ]; then
  echo "usage: scripts/release.sh vX.Y.Z [--push]" >&2; exit 2
fi
if [ -n "$(git status --porcelain)" ]; then
  echo "release: the working tree is not clean" >&2; exit 1
fi
for t in "$version" "mysql/$version" "cmd/sqlshape/$version"; do
  if git rev-parse -q --verify "refs/tags/$t" >/dev/null; then
    echo "release: tag $t already exists" >&2; exit 1
  fi
done
bare=${version#v}; bare=${bare%%-*}
if ! grep -q "^## \[$bare\]" CHANGELOG.md; then
  echo "release: CHANGELOG.md has no section '## [$bare]'" >&2; exit 1
fi

(cd mysql && go mod edit -require="github.com/kr9ly/sqlshape@$version")
(cd cmd/sqlshape && go mod edit -require="github.com/kr9ly/sqlshape@$version" -require="github.com/kr9ly/sqlshape/mysql@$version")
gofmt -l . >/dev/null
go build ./... ./cmd/sqlshape/... ./mysql/...

git add mysql/go.mod cmd/sqlshape/go.mod
git commit -q -m "release: $version" || true   # nothing to commit when the requires already said so
for t in "$version" "mysql/$version" "cmd/sqlshape/$version"; do
  git tag -a "$t" -m "sqlshape $version"
done
echo "release: tagged $version, mysql/$version, cmd/sqlshape/$version at $(git rev-parse --short HEAD)"

if [ "$push" = --push ]; then
  git push origin HEAD "$version" "mysql/$version" "cmd/sqlshape/$version"
else
  echo "release: to publish, run: git push origin HEAD $version mysql/$version cmd/sqlshape/$version"
fi
