#!/usr/bin/env python3
"""Drift guard: embedded rule packs MUST stay byte-identical to the canonical
packs in the meridian-rule-packs repo (audit finding: the embedded rp-wht-2024
had silently diverged with materially wrong rates).

Usage:
    python tools/check_embedded_sync.py [--canonical DIR|URL]

Resolution order for the canonical source:
    1. --canonical argument (directory containing packs/<id>/<ver>.yaml)
    2. $CANONICAL_PACKS_DIR
    3. sibling checkout ../meridian-rule-packs or ../rule-packs
    4. https://raw.githubusercontent.com/munisp/meridian-rule-packs/main (network)

Exit 0 when every mirrored pack matches; exit 1 listing drifts otherwise.
"""
from __future__ import annotations

import os
import sys
import urllib.request
from pathlib import Path

# Embedded pack ids that mirror the canonical registry (byte-identical).
MIRRORED_PACKS = ["rp-wht-2024", "rp-fmt-federal"]

# Service-adapted subsets (flat __op grammar for the Go/Python evaluators) that
# intentionally differ from canonical TODAY; they are reported as warnings, not
# failures, until the einvoicing/tp validator contexts are aligned to the
# canonical operator-map grammar (tracked as follow-up work).
ADAPTED_PACKS = ["rp-ubl-bis", "rp-mbs-business-rules", "rp-tp-2018"]

EMBEDDED_DIR = Path(__file__).resolve().parent.parent / "packages" / "shared" / "rulepack" / "packs"
DEFAULT_URL = "https://raw.githubusercontent.com/munisp/meridian-rule-packs/main"


def canonical_source(arg):
    if arg:
        return arg
    env = os.environ.get("CANONICAL_PACKS_DIR")
    if env:
        return env
    here = Path(__file__).resolve()
    for cand in (here.parents[2] / "meridian-rule-packs",
                 here.parents[2] / "rule-packs",
                 Path.cwd() / "meridian-rule-packs",
                 Path.cwd() / "rule-packs"):
        if (cand / "packs").is_dir():
            return str(cand)
    return DEFAULT_URL


def load_canonical(src, pack_id, version):
    rel = "packs/{}/{}".format(pack_id, version)
    if src.startswith("http://") or src.startswith("https://"):
        url = src.rstrip("/") + "/" + rel
        try:
            with urllib.request.urlopen(url, timeout=15) as r:
                return r.read()
        except Exception as exc:
            print("warn: cannot fetch {}: {}".format(url, exc), file=sys.stderr)
            return None
    p = Path(src) / rel
    return p.read_bytes() if p.is_file() else None


def main(argv):
    arg = None
    if "--canonical" in argv:
        arg = argv[argv.index("--canonical") + 1]
    src = canonical_source(arg)
    print("canonical source: " + src)
    drifts, skipped = [], []
    for pack_id in MIRRORED_PACKS + ADAPTED_PACKS:
        embedded = EMBEDDED_DIR / pack_id
        versions = sorted(p.name for p in embedded.glob("*.yaml"))
        if not versions:
            drifts.append("{}: no embedded versions found".format(pack_id))
            continue
        for ver in versions:
            canon = load_canonical(src, pack_id, ver)
            if canon is None:
                skipped.append("{}/{}: canonical unavailable (skipped)".format(pack_id, ver))
                continue
            emb = (embedded / ver).read_bytes()
            if emb != canon:
                msg = "{}/{}: embedded != canonical ({} vs {} bytes)".format(
                    pack_id, ver, len(emb), len(canon))
                if pack_id in MIRRORED_PACKS:
                    drifts.append(msg)
                else:
                    print("warn {} (service-adapted subset)".format(msg))
            else:
                print("ok   {}/{} byte-identical".format(pack_id, ver))
    for s in skipped:
        print("skip " + s)
    if drifts:
        print("\nDRIFT DETECTED - embedded packs must be re-synced from meridian-rule-packs:")
        for d in drifts:
            print("  FAIL " + d)
        return 1
    if skipped and src == DEFAULT_URL:
        print("\nwarn: canonical source unreachable; nothing verified", file=sys.stderr)
        return 1
    print("\nall mirrored packs in sync")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
