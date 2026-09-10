#!/usr/bin/env sh
# Checks out PostgreSQL's regression corpus (src/test/regress of the release the oracle
# runs) into the sqlshape cache directory, where TestRegress finds it:
#   ${XDG_CACHE_HOME:-$HOME/.cache}/sqlshape/regress-<major>
# Set SQLSHAPE_REGRESS to point the test elsewhere. Re-running is a no-op once fetched.
set -eu
tag=${1:-REL_17_5}
major=$(printf '%s' "$tag" | sed 's/^REL_\([0-9]*\)_.*/\1/')
base=${XDG_CACHE_HOME:-$HOME/.cache}/sqlshape
dest=$base/regress-$major
if [ -f "$dest/src/test/regress/parallel_schedule" ]; then
  echo "regress corpus present: $dest ($(git -C "$dest" describe --tags 2>/dev/null || echo untagged))"
  exit 0
fi
mkdir -p "$base"
rm -rf "$dest.tmp"
git clone -q --depth 1 --filter=blob:none --sparse --branch "$tag" https://github.com/postgres/postgres.git "$dest.tmp"
git -C "$dest.tmp" sparse-checkout set src/test/regress
mv "$dest.tmp" "$dest"
echo "regress corpus fetched: $dest ($tag)"
