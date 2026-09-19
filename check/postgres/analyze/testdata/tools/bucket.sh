#!/bin/bash
# bucket.sh "<CLASS> <key>" [N] — print the first N hits of one summary bucket from $REPORT
R=${REPORT:-report.txt}
awk -v want="==== $1 (" -v n="${2:-20}" '
  index($0, want) == 1 { on = 1; next }
  /^==== / { on = 0 }
  on && /^--- .* #[0-9]+$/ { c++ }
  on && c <= n { print }
' "$R"
