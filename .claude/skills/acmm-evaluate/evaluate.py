#!/usr/bin/env python3
"""Evaluate local checkouts against Hive's ACMM criteria.

Mirrors src/pkg/dashboard/api_acmm_eval.go: a criterion passes when any of its
patterns exists (trailing "/" means directory); a level passes at
acmmLevelThreshold; the codebase level is the highest consecutive passing
level, with a passing L0 counting as L1.

--root SUB mirrors governor.acmm.repo_roots: every pattern is also tried at
SUB/<pattern>, so a checkout that keeps its module under src/ is scored the
way a hive configured with `repo_roots: {<repo>: src}` scores it.
"""
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
CRITERIA = os.path.join(ROOT, "src", "pkg", "dashboard", "acmm_criteria.go")
EVAL = os.path.join(ROOT, "src", "pkg", "dashboard", "api_acmm_eval.go")


def load():
    src = open(CRITERIA, encoding="utf-8").read()
    crit = []
    for m in re.finditer(
        r'\{ID:\s*"([^"]+)".*?Level:\s*(\d+).*?Name:\s*"([^"]+)".*?Patterns:\s*\[\]string\{([^}]*)\}',
        src, re.S,
    ):
        crit.append((m.group(1), int(m.group(2)), m.group(3), re.findall(r'"([^"]+)"', m.group(4))))
    thr = float(re.search(r"acmmLevelThreshold\s*=\s*([0-9.]+)", open(EVAL, encoding="utf-8").read()).group(1))
    return crit, thr


def exists(root, p):
    fp = os.path.join(root, p.rstrip("/"))
    return os.path.isdir(fp) if p.endswith("/") else os.path.exists(fp)


def variants(sub, p):
    """acmmPatternVariants: the pattern at the root, plus under sub when set."""
    return [p] if not sub else [p, sub.strip("/") + "/" + p]


def evaluate(root, crit, thr, sub=""):
    levels = sorted({c[1] for c in crit})
    res = {l: [0, 0] for l in levels}
    fails = []
    for cid, lvl, name, pats in crit:
        ok = any(exists(root, v) for p in pats for v in variants(sub, p))
        res[lvl][1] += 1
        res[lvl][0] += int(ok)
        if not ok:
            fails.append((lvl, cid, name, pats))
    passed = {l: res[l][0] / res[l][1] >= thr for l in levels}
    code = 0
    for l in levels:
        if passed[l]:
            code = l
        else:
            break
    if code == 0 and passed.get(0):
        code = 1
    return levels, res, passed, code, fails


def main(argv):
    if not argv:
        print(__doc__)
        return 2
    sub = ""
    if "--root" in argv:
        i = argv.index("--root")
        if i + 1 >= len(argv):
            print("--root needs a subdirectory argument", file=sys.stderr)
            return 2
        sub = argv[i + 1]
        argv = argv[:i] + argv[i + 2:]
    if not argv:
        print(__doc__)
        return 2
    crit, thr = load()
    for root in argv:
        levels, res, passed, code, fails = evaluate(os.path.abspath(root), crit, thr, sub)
        where = f" (also probing {sub.strip('/')}/)" if sub else ""
        print(f"{root}: codebase level L{code} (threshold {thr}){where}")
        for l in levels:
            print(f"  L{l}: {res[l][0]}/{res[l][1]} {'pass' if passed[l] else 'FAIL'}")
        for lvl, cid, name, pats in fails:
            print(f"  missing L{lvl} {cid} ({name}): any of {', '.join(pats)}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
