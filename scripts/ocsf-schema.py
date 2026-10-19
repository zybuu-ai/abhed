#!/usr/bin/env python3
"""Regenerate ocsf/schema/subset.json from the published OCSF schema server.

The subset holds, for each OCSF version Abhed writes, the classes and objects
the mapper uses: each attribute's type, object type, requirement and enum
values. The mapper's tests check every event it writes against it, and the
downgrade uses it to drop what an older version does not define.

Usage: python3 scripts/ocsf-schema.py [base-url]   (default https://schema.ocsf.io)
"""
import json
import sys
import urllib.request

VERSIONS = ["1.8.0", "1.5.0", "1.3.0", "1.1.0"]
CLASSES = ["base_event", "file_activity", "process_activity", "detection_finding",
           "network_activity", "http_activity", "ssh_activity",
           "device_config_state_change", "application_lifecycle", "api_activity"]
OBJECTS = ["metadata", "product", "actor", "user", "session", "network_endpoint",
           "device", "api", "service", "request", "response", "ai_model",
           "message_context", "process", "file", "http_request", "http_response",
           "url", "network_connection_info", "network_traffic", "finding_info",
           "analytic", "policy", "firewall_rule", "fingerprint", "resource_details"]


def get(base, path):
    with urllib.request.urlopen(base + path, timeout=120) as r:
        return json.load(r)


def attrs(raw):
    # Older servers list attributes as single-key objects; newer ones as a map.
    if isinstance(raw, list):
        out = {}
        for item in raw:
            out.update(item)
        raw = out
    res = {}
    for name, a in sorted(raw.items()):
        e = {"type": a.get("type", "")}
        if a.get("object_type"):
            e["object"] = a["object_type"]
        if a.get("is_array"):
            e["array"] = True
        if a.get("requirement"):
            e["req"] = a["requirement"]
        # Older servers name one profile, newer ones a list.
        profiles = a.get("profiles") or ([a["profile"]] if a.get("profile") else [])
        if profiles:
            e["profiles"] = sorted(profiles)
        if a.get("enum"):
            e["enum"] = sorted(a["enum"].keys(), key=lambda k: (len(k), k))
        res[name] = e
    return res


def main():
    base = sys.argv[1] if len(sys.argv) > 1 else "https://schema.ocsf.io"
    out = {"source": base, "versions": {}}
    for v in VERSIONS:
        ver = {"profiles": sorted(get(base, f"/{v}/api/profiles").keys()), "classes": {}, "objects": {}}
        for c in CLASSES:
            d = get(base, f"/{v}/api/classes/{c}")
            ver["classes"][str(d.get("uid", 0))] = {
                "name": c, "caption": d.get("caption", ""), "profiles": sorted(d.get("profiles") or []),
                "attributes": attrs(d["attributes"])}
        for o in OBJECTS:
            try:
                d = get(base, f"/{v}/api/objects/{o}")
            except Exception:
                continue
            if "error" in d or "attributes" not in d:
                continue
            ver["objects"][o] = {"attributes": attrs(d["attributes"])}
        out["versions"][v] = ver
    json.dump(out, sys.stdout, indent=1, sort_keys=True)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
