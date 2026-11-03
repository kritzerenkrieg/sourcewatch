#!/usr/bin/env python3
"""Validate DECISIONS.md and ROADMAP.md. Exits non-zero on any error."""
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
STATUSES = {"Decided", "Inspect", "TBD", "Question"}
CODE = re.compile(r"^[A-Z]\d+$")
CODE_ANY = re.compile(r"\b[A-Z]\d+\b")
errors = []


def cells(line):
    return [c.strip() for c in line.strip().strip("|").split("|")]


def read(name):
    path = ROOT / "docs" / name
    if not path.exists():
        errors.append(f"{name}: missing")
        return []
    return path.read_text(encoding="utf-8").splitlines()


phases, rows = {}, {}
for line in read("DECISIONS.md"):
    if not line.startswith("|"):
        continue
    c = cells(line)
    if len(c) == 2 and re.fullmatch(r"[A-Z]", c[0]):
        phases[c[0]] = c[1]
    elif len(c) == 4 and CODE.match(c[0]):
        code, _topic, status, text = c
        if code in rows:
            errors.append(f"DECISIONS.md: duplicate code {code}")
        rows[code] = (status, text)

for code, (status, text) in rows.items():
    if code[0] not in phases:
        errors.append(f"DECISIONS.md: {code} uses letter {code[0]} not in the phase table")
    if status not in STATUSES:
        errors.append(f"DECISIONS.md: {code} has invalid status '{status}'")
    if status == "Decided" and not text:
        errors.append(f"DECISIONS.md: {code} is Decided with no text")

current = None
for n, line in enumerate(read("ROADMAP.md"), 1):
    m = re.match(r"##\s+Phase\s+([A-Z])\b", line)
    if m:
        current = m.group(1)
        if current not in phases:
            errors.append(f"ROADMAP.md:{n}: phase letter {current} not in DECISIONS.md")
        continue
    if not re.match(r"\s*- \[[ xX]\]", line):
        continue
    for code in CODE_ANY.findall(line):
        if code not in rows:
            errors.append(f"ROADMAP.md:{n}: unknown code {code}")
        elif (
            current not in (None, "A")
            and "[ ]" in line
            and rows[code][0] != "Decided"
        ):
            errors.append(f"ROADMAP.md:{n}: {code} is {rows[code][0]}, must be Decided before phase {current}")
    if "Done when:" not in line:
        errors.append(f"ROADMAP.md:{n}: task has no 'Done when:' check")

for e in errors:
    print(e)
print(f"{len(rows)} decisions, {len(errors)} error(s)")
sys.exit(1 if errors else 0)
