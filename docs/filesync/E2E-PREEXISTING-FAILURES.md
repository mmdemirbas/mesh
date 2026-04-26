# E2E pre-existing failures (snapshot 2026-04-26)

## Context

After the D4 SQLite cutover landed, `task check` surfaced four failing
e2e tests. To attribute them, the same four tests were run against
`1f3be50` — the last commit before the D4 cutover began — and against
the current `main` HEAD.

## Results

| Test                    | 1f3be50 (pre-D4) | HEAD (post-D4) | Verdict             |
| ----------------------- | ---------------- | -------------- | ------------------- |
| TestFilesyncSendOnly    | PASS (91s)       | PASS (62s)     | Green at HEAD       |
| TestFilesyncMeshC6      | FAIL (107s)      | FAIL (107s)    | Pre-existing        |
| TestFilesyncTwoPeer     | FAIL (153s)      | FAIL (153s)    | Pre-existing        |
| TestGateway             | FAIL (1.3s)      | FAIL (1.3s)    | Pre-existing        |

`git diff 537495b..HEAD -- internal/filesync e2e/` is empty: the D4 fix
commit (`537495b`) was the last touch on filesync or the e2e tree, so
the HEAD numbers above also describe the D4 fix point.

## Conclusion

**The D4 cutover did not introduce any e2e regressions.** All three
currently-failing tests already failed at `1f3be50`.

`TestFilesyncSendOnly` was reported as failing in the original `task
check` run that motivated this audit, but is now reliably green at
both points. The earlier failure was likely transient.

## Failure modes (for follow-up triage)

- **TestFilesyncMeshC6** — peer1 records `mesh_filesync_index_exchanges_total{folder="shared"} 12`
  but `mesh_filesync_files_downloaded_total{folder="shared"} 0`.
  The probe (`v2 from peer1`) never propagates back to peer1 within
  75 seconds. Index exchange runs but the diff produces zero actions.

- **TestFilesyncTwoPeer** — concurrent edits on peer1 and peer2 do not
  produce any `.sync-conflict-*` file within 60 seconds. Either both
  peers converge silently (data loss) or neither writes the rename.

- **TestGateway** — fails in 1.25s with
  `container ... is not running`: the `gw` mesh container starts but
  exits before the test can `exec` into it. Likely a config-validation
  failure or a missing env var; no mesh log is produced.

## Recommendation

Triage these as a separate workstream. They predate D4 and are not
gating the cutover. Each should be reproduced individually with
`go test -tags e2e -run <name> -v` and the per-container artefacts
under `e2e/build/artifacts/<test>/<timestamp>/` inspected.
