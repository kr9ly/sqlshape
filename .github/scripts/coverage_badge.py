#!/usr/bin/env python3
"""Write a shields-style coverage badge SVG from a Go cover profile.

Usage: coverage_badge.py [--min PCT] <cover.out> <output.svg>

The percentage is the statement coverage `go tool cover -func` reports as total. With --min,
exit 1 when it is below PCT so the badge doubles as a regression gate; the SVG is written
either way.
"""
import subprocess
import sys

TEMPLATE = """\
<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="20" role="img" aria-label="coverage: {value}">
  <title>coverage: {value}</title>
  <linearGradient id="s" x2="0" y2="100%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>
  <clipPath id="r"><rect width="{w}" height="20" rx="3" fill="#fff"/></clipPath>
  <g clip-path="url(#r)">
    <rect width="{lw}" height="20" fill="#555"/>
    <rect x="{lw}" width="{vw}" height="20" fill="{color}"/>
    <rect width="{w}" height="20" fill="url(#s)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="110" text-rendering="geometricPrecision">
    <text x="{lx}" y="150" transform="scale(.1)" fill="#010101" fill-opacity=".3">coverage</text>
    <text x="{lx}" y="140" transform="scale(.1)">coverage</text>
    <text x="{vx}" y="150" transform="scale(.1)" fill="#010101" fill-opacity=".3">{value}</text>
    <text x="{vx}" y="140" transform="scale(.1)">{value}</text>
  </g>
</svg>
"""


def color(pct: float) -> str:
    if pct >= 90:
        return "#4c1"
    if pct >= 80:
        return "#97ca00"
    if pct >= 70:
        return "#dfb317"
    if pct >= 60:
        return "#fe7d37"
    return "#e05d44"


def main() -> None:
    args = sys.argv[1:]
    min_pct = None
    if args and args[0] == "--min":
        min_pct = float(args[1])
        args = args[2:]
    profile, out = args
    report = subprocess.run(["go", "tool", "cover", "-func=" + profile], check=True, capture_output=True, text=True).stdout
    total = [l for l in report.splitlines() if l.startswith("total:")][-1]
    pct = float(total.split()[-1].rstrip("%"))
    value = f"{pct:.1f}%"
    label_width = 61
    value_width = 12 + 8 * len(value)
    with open(out, "w") as f:
        f.write(TEMPLATE.format(value=value, color=color(pct), w=label_width + value_width, lw=label_width,
                                vw=value_width, lx=label_width * 5, vx=(label_width + value_width / 2) * 10))
    print(value)
    if min_pct is not None and pct < min_pct:
        print(f"coverage {value} is below the required minimum {min_pct:.1f}%", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
