# About this fork

`tuna-os/hive` is a fork of [`kubestellar/hive`](https://github.com/kubestellar/hive).
It exists so that the Tuna OS organization can run the Hive agent fleet against
its own repositories and container registry. It is **not** a separate project,
and it is not a place to develop new Hive features.

Read this page before opening an issue or a pull request here: several documents
in this repository are inherited from upstream unchanged and describe the
upstream project, not this fork.

## Intent: track upstream, keep the delta near zero

The fork tracks the upstream `v4` branch. The goal is to carry **no** local
patches beyond what is unavoidably org-specific, and to upstream anything that
is not.

Current delta against `kubestellar:v4`:

| Commit | Change | Disposition |
| --- | --- | --- |
| `4955b9f` | Publish images to `ghcr.io/${{ github.repository_owner }}` instead of the hardcoded upstream org | Generic to any fork — should be upstreamed, tracked in [#2](https://github.com/tuna-os/hive/issues/2) and [#10](https://github.com/tuna-os/hive/issues/10) |

Every entry in that table is a liability, not an asset: a local patch has to be
rebased on every sync, and it grows a merge-conflict surface as upstream moves.
A change that would benefit any fork belongs upstream. A change that is
genuinely specific to Tuna OS belongs in configuration, not in a patch.

## Staying current with upstream

Upstream `v4` is fast-moving — on 2026-09-02 it landed 96 commits in a single
day. A fork that is synced by hand is behind by hundreds of commits within a
week, which means upstream bug fixes and security hardening reach the Tuna OS
fleet on no defined schedule.

A drift budget and an automated sync are proposed but **not yet adopted**; the
decision is tracked in [#18](https://github.com/tuna-os/hive/issues/18). Until
it lands, treat "how far behind is this fork?" as a question to answer before
relying on any upstream fix being present:

```sh
gh api repos/tuna-os/hive/compare/kubestellar:v4...tuna-os:v4 \
  --jq '{ahead: .ahead_by, behind: .behind_by}'
```

## Where to send things

| You have | Send it |
| --- | --- |
| A bug or feature idea in Hive itself | Upstream: [`kubestellar/hive` issues](https://github.com/kubestellar/hive/issues) |
| A change that any fork would want | Upstream, as a pull request |
| A security vulnerability in Hive code | Upstream, via private vulnerability reporting on `kubestellar/hive` |
| Something specific to how Tuna OS runs its fleet — registry, org configuration, the fork's own sync | Here |

`SECURITY.md` in this repository is upstream's text. It directs reporters to
this repository's Security tab and says reports are handled by the Hive
Maintainer Committee — those two statements do not both hold for a fork. A
report filed here reaches the fork owner only. Vulnerabilities in Hive code
should go upstream.

## Documents that describe upstream, not this fork

These files are inherited verbatim and are kept unedited so the fork stays cheap
to rebase. Read them as upstream's, and scope them with this page:

- `OWNERS` — the KubeStellar Maintainer Committee. It governs merges upstream,
  not in this fork.
- `GOVERNANCE.md` — upstream's decision-making process, including the RFC and
  supermajority rules.
- `ROADMAP.md` — upstream's release-line trajectory (v4, v5). This fork has no
  release line of its own.
- `ADOPTERS.md` — upstream's adopter roster, which lists Tuna OS as an adopter
  of the upstream project.
- `SECURITY.md` — upstream's disclosure policy; see the routing table above.
- `CONTRIBUTING.md` and `CODE_OF_CONDUCT.md` — upstream's, and they apply to
  contributions sent upstream.

## Ownership

The fork is maintained by the Tuna OS organization. Decisions about the fork —
whether to sync, what delta to carry, whether to keep the fork at all — are made
here. Decisions about Hive itself are made upstream.
