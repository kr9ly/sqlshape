#!/usr/bin/env bash
# Smoke test for a built sqlshape binary: it runs as a vettool, the examples pass, and a
# wrong statement is reported (so the checker really ran and did not just stay silent).
#   .github/scripts/smoke.sh ./sqlshape [expected-version]
set -euo pipefail
bin="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
if [ -n "${2:-}" ]; then
  "$bin" version | grep -F -- "$2"
fi
go vet -vettool="$bin" ./examples/...

probe=examples/1-tables/zz_smoke_test.go
trap 'rm -f "$probe"' EXIT
cat > "$probe" <<'GO'
package tables

import "github.com/kr9ly/sqlshape/v2"

var smoke = sqlshape.Query[int64, struct{}](`SELECT no_such_column FROM customers`)
GO
if go vet -vettool="$bin" ./examples/1-tables 2> smoke.err; then
  echo "smoke: the checker accepted a statement that names a column the schema does not have" >&2
  exit 1
fi
grep -q 'sqlshape: .*no_such_column' smoke.err || { cat smoke.err >&2; exit 1; }
rm -f smoke.err
echo "smoke: ok"
