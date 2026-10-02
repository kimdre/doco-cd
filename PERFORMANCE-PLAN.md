# Performance improvement plan

Handover notes for continuing the deployment performance work on another machine.
This file is not meant to be merged; delete the `docs/performance-plan` branch once the work is done.

## Problem

Last analysed webhook run (`kimdre/homelab`, 18 stacks, 17 skipped): **37.25 s total**, of which the Docker deploy
itself was only **3.69 s**.

| Metric (baseline) | Value |
|---|---|
| Run duration | 37.25 s |
| `cached_project_preflight` (skip path) mean | 6.43 s |
| `git_ancestry` mean | 3.28 s |
| Pre-deploy admission wait | 4.53 s |
| Pre-deploy → deploy gap | 4–15 s |
| CPU / allocations per run | ~63 CPU-s, ~495 MiB |
| `kimdre/homelab` mirror | **1631 packs**, 14 MB on disk (3.7 MB pack data), 0 loose objects |

**Root cause:** go-git never repacks, so every fetch with new objects adds a pack to the bare mirror. `WithMirrorRead`
opens a fresh go-git handle per read, and each fresh handle's first object read loads **every `.idx`**
(~2.3 s uncontended, ~5 s under contention), × 40–60 cold handles per run.

Secondary findings: `GetShortestUniqueCommitHash` inflated every commit; notification metadata was built even when
nothing was sent; Apprise requests had no HTTP timeout; commit statuses used the mirror's moving branch head instead of
the job's revision; ancestry walks followed second parents first; all duration histograms topped out at 10 s.

## Status

| # | PR | Branch | Status |
|---|---|---|---|
| 1 | #1945 fix(metrics): extend duration histogram buckets and instrument mirror reads | `fix/prometheus-duration-buckets` | ✅ merged |
| 2 | #1946 perf(git): compact bare mirror packfiles after fetch | `perf/git-mirror-pack-compaction` | ✅ merged |
| 3 | #1947 perf(git): look up short commit hashes in pack indexes | `perf/git-short-commit-hash` | ✅ merged |
| 4 | #1948 perf(stages): walk deployment ancestry newest commit first | `perf/stages-ancestry-walk` | 🟡 open |
| 5 | #1949 fix(notification): time out Apprise requests after 30 seconds | `fix/notification-http-timeout` | 🟡 open |
| 6 | #1950 fix(stages): post commit statuses for the published revision | `fix/commit-status-revision` | 🟡 open (stack root) |
| 7 | #1951 perf(stages): skip notification commit lookups that are not delivered | `perf/notification-metadata` | 🟡 open, **stacked on #1950** |
| 8 | perf(stages): report deployment start without blocking the deployment | `perf/async-deploy-reporting` | ⏸️ deferred until NAS validation |
| 9 | perf(config): resolve deploy configs with one mirror handle | `perf/config-single-mirror-read` | ⏳ not started, low priority |
| 10 | perf(git): reuse warm mirror handles | `perf/git-mirror-handle-pool` | ⏳ conditional, only if still needed |
| – | #1953 feat(api): endpoint to compact git mirrors with a full re-encode | – | 📝 issue, later |

## Next steps

1. **Merge the open PRs** (#1948, #1949, #1950, then #1951). PRs are squash-merged.
   - After #1950 is merged, rebase the stacked #1951 and retarget it to `main`:
     ```sh
     git fetch origin
     git switch perf/notification-metadata
     git rebase --onto origin/main fix/commit-status-revision
     git push --force-with-lease
     gh pr edit 1951 --base main
     ```
   - #1951 conflicts with `main` in `internal/stages/stage_4_post-deploy.go` (the mirror read instrumentation from
     #1945). Resolve it during the rebase, keeping both the instrumentation and the notification gating, then rerun
     `go test -race ./internal/stages/... ./internal/notification/...`.
   - #1948, #1949 and #1950 merge cleanly with the current `main`.
2. **Validate on the NAS** with a dev build containing all merged PRs (see [NAS validation](#nas-validation)).
3. **Decide the remaining PRs** from the validation results:
   - PR 8 (async reporting): only if the pre-deploy → deploy gap is still noticeably above ~1 s.
   - PR 9 (single mirror read in `GetConfigs`): only if config reads still show up in run timings.
   - PR 10 (warm handle pool): only if mirror reads are still slow with a single pack.
4. **Later:** implement #1953 (manual re-encode compaction endpoint).

## NAS validation

- Metrics endpoint: `http://192.168.0.12:9120/metrics`
- Loki (Grafana): datasource UID `cflfb4pcfptdsb`, label `container_name="doco-cd"` (not `container`).
- Mirror on the NAS: `/var/lib/docker/volumes/doco-cd_data/_data/github.com/kimdre/homelab/mirror`

Checklist after deploying the dev build:

- [ ] `doco_cd_git_mirror_packs{repository="github.com/kimdre/homelab"}` drops from 1631 to **1** after the first fetch.
- [ ] `doco_cd_git_mirror_compactions_total{result="compacted"}` increments once; check
      `doco_cd_git_mirror_compaction_duration_seconds` (expected: well under a few seconds).
- [ ] Mirror pack directory is ~4 MB:
      ```sh
      m=/var/lib/docker/volumes/doco-cd_data/_data/github.com/kimdre/homelab/mirror
      ls $m/objects/pack/*.idx | wc -l; du -sh $m/objects/pack
      ```
- [ ] Compare a comparable webhook run against the [baseline](#problem): total run duration,
      `cached_project_preflight`, `git_ancestry`, admission wait, pre-deploy → deploy gap, post-deploy duration.
- [ ] Check `doco_cd_git_mirror_lock_wait_seconds` and the `deployed_commit_lookup` pre-deploy operation from #1945.
- [ ] Repeat once all PRs are merged.

## Remaining work

### PR 8 — `perf/async-deploy-reporting` (deferred, stacked on #1951)

Loki showed the pre-deploy → deploy gap is 4–15 s, mostly cold mirror reads that PRs 2, 3, 6 and 7 remove. Only
about 2 × 0.4–0.5 s of HTTP posts (started notification, Queued/In Progress statuses) remain on the deploy path.

If still worth it:
- Per-StageManager FIFO reporting worker for the started notification and the Queued/In Progress statuses; snapshot
  inputs at enqueue time.
- `inProgressPosted` becomes a future the phase reporter awaits before its first post; guard `commitStatusTarget`
  with a mutex.
- Drain the worker before the final Success/failure status, the failure notification, `selfUpdateCommitStatus` and
  early returns; cancellation drops pending items.
- The self-update handover needs a lazily resolved `DeployRequest.CommitStatus`.
- Tests (`-race`): ordering Queued → In Progress → phases → final; drain on failure; cancellation; handover.

### PR 9 — `perf/config-single-mirror-read` (low priority)

- `GetConfigs` (`internal/config/deploy/deploy.go`): merge the mirror read regions into one handle and drop redundant
  `PlainOpen` calls.

### PR 10 — `perf/git-mirror-handle-pool` (conditional)

- Only if mirror reads are still slow after compaction: warm handle pool keyed by pack-set signature plus a generation
  counter bumped on exclusive unlock; one borrower per handle; bounded with idle eviction; stress tests under `-race`.

### #1953 — manual re-encode compaction

- `POST /v1/api/mirrors/compact` (+ MCP tool), `mode=repack|copy`, optional `repository` filter, `202 Accepted`.
- go-git `packfile.Encoder` with storage opened with `filesystem.Options{KeepDescriptors: true}`; reuse `writePack`
  and `verifyPack` from commit `813264ac` (first version of #1946).
- One mirror at a time; skip mirrors whose lock is busy; keep old packs until the new index is verified.

## Mirror compaction notes (#1946)

- Triggered in `CloneOrUpdateBareMirror` under the exclusive mirror lock once a mirror has **> 32 packs**
  (`mirrorCompactPackThreshold`); never fails the fetch; a failed mirror is retried after 1 h.
- `internal/git/mirror_pack_concat.go` copies the compressed pack entries instead of re-encoding:
  - Oldest pack wins for duplicate objects; each copied entry is checked against its source index CRC-32.
  - `OFS_DELTA` stays when its base comes from the same pack, otherwise becomes `REF_DELTA`; a delta whose base comes
    from a newer pack is written as a full object (keeps delta chains acyclic).
  - `.idx` is installed before `.pack` (go-git discovers packs by `.pack`).
- No size cap: memory scales with object count (~280 B/object peak), duration with disk throughput.
- go-git filters out the `thin-pack` capability, so fetched packs are self-contained and store changed files as full
  objects. That is why `git repack -f` shrinks the homelab mirror to 1.4 MB while the copy approach gives ~4 MB.

Benchmark (M1 Max, mirrors fragmented with `git repack --max-pack-size`, all `git fsck --full` clean):

| Mirror | Packs size | go-git encoder: time / peak heap | Entry copy: time / peak heap |
|---|---|---|---|
| go-git | 12 MiB | 10 s / 208 MB | 0.26 s / 20 MB |
| docker/compose | 38 MiB | 25 s / ~500 MB | 0.8 s / 34 MB |
| prometheus | 362 MiB | 2 m 26 s / 2.06 GB | 1.8 s / 58 MB |
| golang/go (714k objects) | 527 MiB | not run | 2.7 s / 201 MB |

The entry copy also compacted prometheus in 1.4 s inside a Linux container limited to 128 MiB of memory.

## Conventions

- One branch per PR, named `type/short-name`, based on `main` (stacked PRs on their parent branch).
- One conventional commit per change, **no `Co-authored-by: Copilot` trailer**. Only commit when explicitly asked.
- PRs via `gh pr create` with a conventional-commit title, label per `.github/release.yml` (`perf` → `enhancement`,
  `fix` → `bug`) and a body with `## Summary` and `## Validation`; stacked PRs mention "Stacked on #N".
- Tests: `WEBHOOK_SECRET="test_Secret1" API_SECRET="test_apiSecret1" go test -race ./internal/<pkg>/...`
- Lint with the repo's pinned linter: `.bin/golangci-lint run --fix ./...` (a globally installed version may miss
  findings).
- With `safe.bareRepository=explicit`, run git against bare mirrors with `git --git-dir=<path> ...`.
