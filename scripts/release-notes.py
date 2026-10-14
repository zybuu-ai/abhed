#!/usr/bin/env python3
"""Print the opening of a release's notes from CHANGELOG.md.

Usage: release-notes.py <tag> [CHANGELOG.md]

The notes open with the version's "Upgrading" list and its "Known limits",
in that order, so neither is lost under a list of pull requests. A version
with no section in the changelog fails, so a tag cannot ship without one; a
pre-release falls back to its version's section, then to Unreleased.
`--self-test` checks the extraction on made-up input and exits.
"""
import re
import sys

KEEP = ("upgrading", "known limits")


def version_section(text, version):
    """Returns the body of `## [version]`, or None."""
    m = re.search(r"^## \[" + re.escape(version) + r"\][^\n]*\n(.*?)(?=^## \[|\Z)", text, re.M | re.S)
    return m.group(1) if m else None


def notes(text, tag):
    version = tag[1:] if tag.startswith("v") else tag
    body = version_section(text, version)
    # A pre-release (1.3.0-rc.1) may use its version's section or Unreleased.
    if body is None and "-" in version:
        body = version_section(text, version.split("-", 1)[0]) or version_section(text, "Unreleased")
    if body is None:
        raise ValueError(f"CHANGELOG.md has no section for {version}")
    out = []
    for part in re.split(r"^(?=### )", body, flags=re.M):
        head = part.split("\n", 1)[0]
        if head.startswith("### ") and head[4:].strip().lower().startswith(KEEP):
            out.append(part.strip())
    return "\n\n".join(out)


def self_test():
    text = """# Changelog

## [Unreleased]

### Added

- new

## [2.0.0] - 2030-01-01

### Upgrading from 1.9

- one

### Security

- fixed

### Known limits

- slow

## [1.9.0] - 2029-01-01

### Upgrading

- old
"""
    got = notes(text, "v2.0.0")
    want = "### Upgrading from 1.9\n\n- one\n\n### Known limits\n\n- slow"
    assert got == want, repr(got)
    assert notes(text, "v1.9.0") == "### Upgrading\n\n- old"
    assert notes(text, "v2.0.0-rc.1") == want
    assert notes(text, "v3.0.0-rc.1") == ""
    try:
        notes(text, "v3.0.0")
    except ValueError:
        pass
    else:
        raise AssertionError("a missing version passed")
    print("release-notes self-test passed")


def main(argv):
    if argv[1:] == ["--self-test"]:
        self_test()
        return 0
    if len(argv) not in (2, 3):
        print(__doc__.strip().splitlines()[2], file=sys.stderr)
        return 2
    path = argv[2] if len(argv) == 3 else "CHANGELOG.md"
    with open(path, encoding="utf-8") as f:
        text = f.read()
    try:
        print(notes(text, argv[1]))
    except ValueError as e:
        print(e, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
