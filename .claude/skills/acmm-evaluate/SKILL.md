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
criterion with the paths that would satisfy it. Detection is root-relative:
a repo that keeps its module under `src/` scores lower than its substance
warrants — that is a known limitation of the criteria, not something to
"fix" by adding empty files.
