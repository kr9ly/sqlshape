#!/usr/bin/env python3
"""hits.py REPORT "CLASS key" [substring] [N] — print whole hits of a bucket whose text contains substring."""
import sys, re
report, bucket = sys.argv[1], sys.argv[2]
sub = sys.argv[3] if len(sys.argv) > 3 else ""
n = int(sys.argv[4]) if len(sys.argv) > 4 else 10
text = open(report).read()
m = re.search(r"^==== " + re.escape(bucket) + r" \(\d+\)\n(.*?)(?=^==== |\Z)", text, re.S | re.M)
if not m:
    sys.exit("no bucket " + bucket)
blocks = re.split(r"^(?=--- \S+ #\d+\n)", m.group(1), flags=re.M)
shown = 0
for b in blocks:
    if not b.strip() or (sub and sub not in b):
        continue
    print(b.rstrip() + "\n")
    shown += 1
    if shown >= n:
        break
print(f"[{shown} shown]")
