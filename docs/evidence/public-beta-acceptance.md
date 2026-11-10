# Public beta acceptance

The release candidate is `0.3.0-beta.1` for Intel macOS. This record separates current live evidence, prior live evidence, and offline tests.

## Live synthetic coding

An MCP client submitted one high-level Go objective with `decompose: true`, a registered `target` check, and the approved source, test and module paths. No worker was assigned by the caller. Ahenk selected signed AGY 1.5.4 and `gemini-3.8-flash-medium` for planning, inspection and implementation. The validated graph contained a read-only inspection followed by implementation. Replaying the same request returned the same parent job.

The trusted-local writer changed only `calc.go`, correcting `RoundDollar` for input 100. The registered `TestWholeDollar` passed with exit status 0. The verified artifact, original file bytes and Git index were checked after execution.

| Evidence | SHA-256 |
| --- | --- |
| Validated plan output | `5740414829725ef236cae5e064aa923bd7aac081147781cc778ec13adc144950` |
| Verified coding diff | `76828d75ab94c8561755aa16a6157698c99cac702ffbb4400751bf52e64b526b` |

The current task did not require independent review. Prior live M2A evidence verifies both cross-provider review directions, including a hash-bound finding on a deliberate defect. The automatic critical-review route was exercised offline with a bound diff, deterministic check result and provider-family exclusion. [M2A review evidence](m2a-acceptance.md)

## Scheduler and project isolation

- Graph insertion and parent transitions are transactional. Fault injection verified rollback on a duplicate child or a missing durable row. A repeated graph expansion is fenced.
- A persisted worker without a cleanup marker blocks dispatch and resume. A persisted cancellation remains in force through restart and becomes terminal once cleanup is confirmed.
- Two independent synthetic Git repositories ran through the same MCP connection. They had separate artifact and snapshot identities, different allowed paths and providers, and different batch, deadline and concurrency policies. A cross-project artifact request and a foreign path were refused. One project could execute concurrently with the other while its second job waited.
- Existing tests cover exclusive SQLite ownership, process cleanup, writer conflicts, retained artifacts, rate-limit scopes and fallback. Prior live OpenCode-to-AGY evidence covers durable checkpoint recovery across a restart. [Checkpoint evidence](../milestones/m2b-agent-orchestration.md#alpha4-native-child-and-checkpoint-acceptance)

## Provider-native blockers

| Goal | Status | Observed blocker |
| --- | --- | --- |
| G-013 | BLOCKED | Signed AGY 1.5.4 reported `invoke_subagent` in state `ERROR` with no child conversation ID, tool result or provider reason. Its separate live capability gate remains closed. |
| G-014 | BLOCKED | The pinned OpenCode 2.2.6 Free GLM native background attempt received a fresh HTTP 429 before an owned child completed. The provider retry boundary was 2026-11-11 00:00 UTC. Offline correlation passed. |

These are provider-native features. The independently verified Ahenk-managed DAG uses its own scheduler and durable job identities.

## Usability gate

| Gate | Result | Evidence |
| --- | --- | --- |
| U01 Clean installation | PASS | Two clean Intel builds produced identical archives. The extracted x86-64 executable reported `0.3.0-beta.1 darwin/amd64`. License and attribution bytes matched. Read-only diagnostics and pinned provider detection worked from isolated state. |
| U02 Codex App MCP integration | PASS | Existing Codex App integration and live M1 evidence were preserved. A fresh stdio client discovered the same eight tools, verified that `decompose` is optional, and retrieved status and results from the current live DAG. [Codex App integration evidence](m1-build.md) |
| U03 Real coding | PASS | Current AGY plan and scoped native edit passed the registered Go check. Prior live independent review supplies the cross-provider evidence while the current Free route remains rate-limited. |
| U04 Failure and recovery | PASS | Offline timeout, cancellation, orphan fencing, transactional fault injection, process cleanup, fallback and cooldown tests passed. Prior live OpenCode-to-AGY checkpoint recovery passed across restart. |
| U05 Multiple projects | PASS | Two synthetic repositories ran under distinct project policies. Cross-project paths and artifact requests were denied. |
| U06 Public defaults | PASS | Restricted defaults, explicit synthetic trusted-local opt-in, pinned provider eligibility and quota guards passed. The security policy describes residual host authority. |
| U07 Quality and results | PASS | Planning used role evidence, registered checks recorded exit status, and independent review remained artifact-bound. Quality, cooldown and capacity regressions passed. |
| U08 Documentation | PASS | The README's isolated fixture workflow, verified diff handoff and readiness checks ran from the extracted binary without a private source configuration or provider credentials. |

`go test -mod=readonly ./...`, `CGO_ENABLED=1 go test -mod=readonly -race ./...`, `go vet -mod=readonly ./...`, and `go mod verify` passed on Intel macOS with Go 1.27.3. Packaging used `GOOS=darwin GOARCH=amd64 CGO_ENABLED=0`. The reproducible archive SHA-256 is `d7f458d73735f1dc95a672a9ae12cce2cb18d7f4e08f86204e10eef1671a427d`.

## Public source audit

The release inventory contains 97 indexed files. It includes the Go sources and fixtures, documentation and sanitized evidence, fixture configuration, build scripts, GitHub templates and CI, the environment example, and the license and attribution files. `git ls-files` lists the exact inventory from the reviewed branch. Runtime state, local credentials and configuration, build archives, databases, scratch directories and raw provider output are excluded. The indexed source passed checks for credential signatures, private host paths, runtime file types and live job or session identifiers. The native protocol tests contain synthetic identifiers only.

Maintained text is English. The copyright holder's name and two legacy Turkish risk keywords in the input classifier preserve attribution and critical-task compatibility. The Apache-2.0 `LICENSE` remains byte-identical to the remote baseline. The binary package carries the dependency license texts and notices identified by the packaging script. [Attribution](../third-party-licenses.md)

The GitHub Actions workflow has read-only permissions and runs Go tests, race checks, vet, module verification, offline MCP and Intel packaging checks. The workflow syntax and local commands were verified before publication.

`CORE_USABILITY=PUBLIC_BETA_READY`

`P0_P1_COMPLETENESS=PARTIALLY_COMPLETE`

The incomplete goals are the two experimental provider-native child routes. The verified core level is `M2B_AUTONOMOUS_ORCHESTRATION_READY`.
