# M2A acceptance

Highest verified readiness is `M2A_TRUSTED_LOCAL_CODING_REVIEW_READY`. The trusted-local acceptance is limited to explicitly opted-in disposable synthetic workspaces. Strict OS containment remains unverified.

## Goal results

| Goal | Status | Evidence |
| --- | --- | --- |
| G-009 | DONE for trusted-local acceptance | The restricted pilot and snapshot-only analysis and review remain available. The opted-in writer used the pinned Headless CLI with observed native reads and edits, scoped per-run permissions, quota preflight and a verified terminal result. |
| G-010 | DONE for trusted-local acceptance | Private detached worktrees, verified snapshot and base, source HEAD and index integrity, scoped file hashes, single-writer leases and cleanup were verified. OS containment is outside this acceptance scope. |
| G-011 | DONE | A real AGY worker edited the synthetic rounding function. The registered Go test passed with exit status 0 and Ahenk independently verified the diff. |
| G-012 | DONE | A real anonymous OpenCode Free worker edited the separate synthetic doubling function. The registered Go test passed with exit status 0 and Ahenk independently verified the diff. |
| G-018 | DONE | Both live directions produced an independent verdict bound to the original artifact. OpenCode identified the intended rounding defect and blocked the batch despite a passing narrow test. |
| G-033 | DONE | Apache-2.0, upstream attribution, English documentation and public repository readiness are preserved. |

## Live coding and independent review

| Writer | Native work | Registered check | SHA-256 | Independent verdict |
| --- | --- | --- | --- | --- |
| AGY 1.5.3, `gemini-3.8-flash-medium` | Three `read_file` calls and one `edit_file` call changed only `calc.go`. | `TestWholeDollar`, PASS, exit 0 | `7f58de12df81e5cde2ec022b76a1537f23d54b407840b954058446514b9f4cd5` | OpenCode Free `opencode/glm-5.3-flash-free`, `CHANGES_REQUESTED` |
| OpenCode 2.2.6, `opencode/glm-5.3-flash-free` | Three `read` calls and one `edit` call changed only `calc.go`. The native run reported zero cost. | `TestDouble`, PASS, exit 0 | `8074c2763ea00c13be398b33899f8270b0a25912c6358c3cd7f90f730b784fc5` | AGY `gemini-3.8-flash-medium`, `APPROVED` |

In the defect fixture, truncation made 50 cents round to zero instead of 100. The registered whole-dollar test passed. OpenCode returned a high-severity finding against the exact artifact and the batch recorded `INDEPENDENT_REVIEW_NOT_APPROVED`. One malformed OpenCode review was retried through `resume_job`. The producer remained completed with one attempt, and the reviewer received the original verified diff and test result.

The AGY preflight verified the existing subscription quota and documented `useG1Credits:false` settings. The CLI did not attest the effective runtime credit setting. Per-run permissions were used without the broad permission bypass. Initial AGY attempts were rejected by the event verifier before the pinned native file event aliases and argument shape were supported. The disposable source checkouts and Git indexes were verified after each attempt.

## End-to-end matrix

| Test | Status | Evidence |
| --- | --- | --- |
| E2E-A | PASS | Live AGY native edit, named Go test, verified diff and unchanged source. |
| E2E-B | PASS | Live OpenCode Free native edit, named Go test, verified diff and unchanged source. |
| E2E-C | PASS | OpenCode independently detected the rounding defect and requested changes against the AGY artifact. |
| E2E-D | PASS | AGY independently approved the OpenCode artifact with passing check evidence. |
| E2E-E | PASS for trusted-local | Offline tests cover conflicting writers, traversal, symlinks, protected writes and Git pointer changes. The pinned OpenCode CLI rejected an external sentinel read and a protected test edit with a local mock inference endpoint. AGY stream tests rejected out-of-scope tool events. The live writes touched only the approved files. Strict host containment remains unverified. |
| E2E-F | PASS | Stdio regression covered shutdown, resume, cancellation and deadlines. Lease tests covered quarantine for uncertain provider and check cleanup. No private worktree remained after the live acceptance. |
| E2E-G | PASS | Current stdio fixture handoff and historical live M1 acceptance were verified separately. [M1 evidence](m1-build.md) |
| E2E-H | PASS | G-033 publication controls and the Intel package remain in place. |

## Regression

- `CGO_ENABLED=0 go test -mod=readonly -count=1 -p=1 ./...` passed.
- `CGO_ENABLED=1 go test -mod=readonly -race -count=1 -p=1 ./...` passed.
- `CGO_ENABLED=0 go vet -mod=readonly -p=1 ./...` and `go mod verify` passed.
- The stdio suite found eight tools and exercised the existing M1 fixture handoff, job persistence, pause, resume and cancellation. The SQLite table schema was preserved. New optional evidence fields are stored in the existing JSON job body.
- `make dist` produced a Mach-O x86_64 executable. Its version output was `ahenk 0.1.0 darwin/amd64`. The extracted Apache license matched the source file byte for byte.

## Open-Source Readiness

The GitHub project is public. Local `origin` is `git@github.com:faoztas/ahenk-mcp.git` and the Go module is `github.com/faoztas/ahenk-mcp`. The local `main` branch has no commits. The repository has no commit history to remediate. Review and separate authorization are required for the initial commit and push.

The root license is the complete Apache License, Version 2.0. Original source files carry `Apache-2.0` SPDX identifiers. The Intel package includes 39 upstream legal files covering 18 linked modules and the Go toolchain, with their original text preserved. The project has no upstream attribution requiring a root NOTICE file. [Third-party inventory](../third-party-licenses.md)

All maintained documentation and user-facing text passed the English audit. The copyright holder's spelling and the legacy Turkish objective classification inputs were retained. CLI names, MCP identifiers and protocol values were preserved.

The publishable source inventory was scanned for credential patterns, private paths, account data and live run identifiers. The scan and document review found no unresolved disclosure. Local configuration, SQLite state, generated outputs, raw transcripts and provider session material are outside the publication inventory. Live acceptance records are summarized without account measurements or machine-specific data.

### Source publication inventory

```text
.env.example
.gitignore
AGENTS.md
CONTRIBUTING.md
LICENSE
Makefile
README.md
SECURITY.md
cmd/ahenk/codexbar.go
cmd/ahenk/m1.go
cmd/ahenk/main.go
cmd/ahenk/main_test.go
cmd/ahenk/opencode.go
docs/evidence/agy-p0.md
docs/evidence/codexbar-p0.md
docs/evidence/m1-build.md
docs/evidence/m2a-acceptance.md
docs/evidence/model-isolation.md
docs/evidence/native-opencode.md
docs/evidence/opencode-p0.md
docs/goal-status.md
docs/milestones/m2a-real-coding-agents.md
docs/third-party-licenses.md
examples/ahenk.fixture.json
go.mod
go.sum
internal/execution/artifact.go
internal/execution/go_check.go
internal/execution/go_check_test.go
internal/execution/process.go
internal/execution/snapshot.go
internal/execution/worktree.go
internal/execution/worktree_test.go
internal/mcpserver/server.go
internal/model/model.go
internal/model/trusted_local_test.go
internal/orchestration/engine.go
internal/orchestration/handoff.go
internal/orchestration/handoff_test.go
internal/orchestration/implementation.go
internal/orchestration/opencode_health.go
internal/orchestration/quota.go
internal/orchestration/quota_test.go
internal/orchestration/review.go
internal/orchestration/review_test.go
internal/providers/agy.go
internal/providers/agy_run.go
internal/providers/agy_run_test.go
internal/providers/agy_test.go
internal/providers/codexbar.go
internal/providers/opencode.go
internal/providers/opencode_catalog.go
internal/providers/opencode_native.go
internal/providers/opencode_native_test.go
internal/providers/opencode_test.go
internal/providers/opencode_trusted.go
internal/providers/opencode_trusted_test.go
internal/providers/providers.go
internal/providers/providers_test.go
internal/providers/quota.go
internal/readiness/m1.go
internal/readiness/m1_test.go
internal/readiness/report.go
internal/readiness/report_test.go
internal/store/opencode_cooldown.go
internal/store/store.go
scripts/dist.sh
```
