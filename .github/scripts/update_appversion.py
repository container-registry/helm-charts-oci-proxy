"""Set appVersion in a Chart.yaml by rewriting that one line.

A YAML round trip (yq -i) reflows the folded description and turns the bot PR
into a conflict with the chart release PR, so the file is edited as text.
"""

import os
import re
import subprocess
import sys

VERSION_RE = re.compile(r"v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?")
LINE_RE = re.compile(r'^(appVersion:\s*)"([^"]*)"(.*)$')


def fail(msg):
    print(f"::error::{msg}")
    sys.exit(1)


def main(path, version):
    if not VERSION_RE.fullmatch(version):
        fail(f"unexpected version {version!r}, want vX.Y.Z[-pre]")
    lines = open(path).read().split("\n")
    hits = [i for i, line in enumerate(lines) if LINE_RE.match(line)]
    if len(hits) != 1:
        fail(f'{path}: want exactly one top-level appVersion: "..." line, found {len(hits)}')
    i = hits[0]
    prefix, current, rest = LINE_RE.match(lines[i]).groups()
    if current == version:
        print(f"appVersion already {version}, nothing to do.")
        return False
    lines[i] = f'{prefix}"{version}"{rest}'
    open(path, "w").write("\n".join(lines))
    stat = subprocess.run(["git", "diff", "--numstat", "--", path], capture_output=True, text=True, check=True).stdout
    if not stat.startswith("1\t1\t"):
        subprocess.run(["git", "diff", "--", path])
        fail(f"{path}: expected a one-line change")
    print(f"appVersion {current} -> {version}")
    return True


if __name__ == "__main__":
    changed = main(sys.argv[1], sys.argv[2])
    with open(os.environ["GITHUB_OUTPUT"], "a") as out:
        out.write(f"changed={'true' if changed else 'false'}\n")
