#!/usr/bin/env python3
"""Consolidate the security scans into one report, and fail on anything live.

Inputs are whatever scan artefacts exist in the directory given; a missing one
is reported as not-run rather than silently passing, because a scanner that
failed to execute must not read as a clean result.

Usage: security-report.py <artifact-dir> [--out report.md]
"""
import json
import pathlib
import sys


def read_json(p: pathlib.Path):
    try:
        return json.loads(p.read_text())
    except Exception:
        return None


def govulncheck(d: pathlib.Path):
    """Source and binary CVEs, split by whether this code can reach them.

    govulncheck emits concatenated pretty-printed JSON objects, not one per
    line. A finding whose top trace frame names a function is called from here
    and blocks; one that names only a module is present but unreachable, which
    is worth reporting and not worth failing a build over.
    """
    p = d / "govulncheck.json"
    if not p.exists():
        return None, [], []

    raw = p.read_text()
    dec, i, msgs = json.JSONDecoder(), 0, []
    while i < len(raw):
        while i < len(raw) and raw[i].isspace():
            i += 1
        if i >= len(raw):
            break
        try:
            obj, i = dec.raw_decode(raw, i)
        except ValueError:
            break
        msgs.append(obj)

    reachable, present = set(), set()
    for m in msgs:
        f = m.get("finding")
        if not f or not f.get("osv"):
            continue
        trace = f.get("trace") or []
        if trace and trace[0].get("function"):
            reachable.add(f["osv"])
        else:
            present.add(f["osv"])
    return True, sorted(reachable), sorted(present - reachable)


def trivy(d: pathlib.Path, name: str):
    """Image or filesystem CVEs. HIGH and CRITICAL block; the rest are noted."""
    p = d / f"{name}.json"
    data = read_json(p)
    if data is None:
        return None, [], []
    blocking, noted = set(), set()
    for res in data.get("Results") or []:
        for v in res.get("Vulnerabilities") or []:
            label = f"{v['VulnerabilityID']} {v.get('PkgName', '')} ({v.get('Severity')})"
            if v.get("Severity") in ("HIGH", "CRITICAL"):
                blocking.add(label)
            else:
                noted.add(label)
    return True, sorted(blocking), sorted(noted)


def main() -> int:
    d = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".")
    out = None
    if "--out" in sys.argv:
        out = pathlib.Path(sys.argv[sys.argv.index("--out") + 1])

    scans = [
        ("Go source and binary CVEs (govulncheck)", *govulncheck(d)),
        ("Dependency CVEs (trivy fs)", *trivy(d, "trivy-fs")),
        ("Container image CVEs (trivy image)", *trivy(d, "trivy-image")),
    ]

    lines = ["# Security scan report", ""]
    failed = False
    for name, ran, blocking, noted in scans:
        if ran is None:
            lines.append(f"- **{name}** — NOT RUN")
            failed = True
            continue
        if blocking:
            lines.append(f"- **{name}** — {len(blocking)} blocking finding(s)")
            lines += [f"    - {f}" for f in blocking[:25]]
            failed = True
        else:
            lines.append(f"- **{name}** — clean")
        if noted:
            lines.append(f"    - {len(noted)} present but not reachable or below the bar: "
                         + ", ".join(noted[:8]))

    lines += ["", "Adversarial suite and the gosec baseline run as ordinary test steps;",
              "their failures fail this workflow directly."]
    report = "\n".join(lines) + "\n"
    print(report)
    if out:
        out.write_text(report)

    if failed:
        print("FAIL: a scan reported a live finding, or did not run.")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
