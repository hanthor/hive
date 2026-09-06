---
name: acmm-evaluate
description: Score a repository checkout against Hive's ACMM criteria (the same file-existence checks the dashboard runs) without a running hive. Use when asked what ACMM level a repo is at, or which criteria block the next level.
---

# acmm-evaluate

Runs the criteria in `src/pkg/dashboard/acmm_criteria.go` against a local
checkout, exactly as `checkCriterion` does (a criterion passes if ANY listed
path exists), and scores levels with the dashboard's threshold.

```bash
python3 .claude/skills/acmm-evaluate/evaluate.py /path/to/repo [more repos...]
```

Prints per-level `matched/total`, the codebase level, and every failing
criterion with the paths that would satisfy it. Detection is root-relative
by default; a repo that keeps its module under `src/` (this one) scores lower
than its substance warrants. Pass `--root src` to also probe `src/<pattern>`,
which is what a hive configured with `governor.acmm.repo_roots: {<repo>: src}`
does (see `src/docs/acmm-policy-matrix.md`). Do not "fix" a low score by
adding empty files at the root.

```bash
python3 .claude/skills/acmm-evaluate/evaluate.py --root src .
```
