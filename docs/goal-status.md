# Goal status

The M1 acceptance run completed G-001 through G-008. Offline fixture evidence and live provider evidence are recorded separately.

| Goal | Status | Evidence or next requirement |
| --- | --- | --- |
| G-001 AGY tool scope | DONE | The restricted profile verified tool inventory, native finish, and denial handling. [Evidence](evidence/agy-p0.md#g-001--done) |
| G-002 AGY cost and identity | DONE | The live subscription check verified documented credit protection. Effective runtime credit protection was `UNKNOWN`. [Evidence](evidence/agy-p0.md#g-002--done) |
| G-003 First live AGY inference | DONE | The pilot verified the model, terminal output, token usage, result hash, and source integrity. [Evidence](evidence/agy-p0.md#g-003--done) |
| G-004 CodexBar accounts and quotas | DONE | Codex and AGY account bindings matched and yielded known quota windows. [Evidence](evidence/codexbar-p0.md) |
| G-005 OpenCode Free recovery | DONE | Native GLM completed the exact-marker smoke with zero reported cost, usage, and cleanup evidence. [Evidence](evidence/native-opencode.md) |
| G-006 Free route health and fallback | DONE | Offline tests covered route isolation, restart, and fallback. The live report showed separate stateless cooldowns and no shared gate. Numeric capacity stayed `UNKNOWN`. [Evidence](evidence/model-isolation.md) |
| G-007 Live multi-provider E2E | DONE | One batch transferred an independently verified AGY diff to the native OpenCode receiver. [Evidence](evidence/m1-build.md) |
| G-008 Readiness and evidence reporting | DONE | Fixture and live readiness were verified in separate states. [Evidence](evidence/m1-build.md) |
| G-009 AGY capability profiles | DONE | Restricted and snapshot-only profiles are preserved. The opt-in AGY writer verified native scoped tools and terminal evidence. [Evidence](evidence/m2a-acceptance.md) |
| G-010 Tool execution and workspace isolation | DONE | Trusted-local worktree identity, source integrity and cleanup verified. Strict host containment remains unverified. [Evidence](evidence/m2a-acceptance.md) |
| G-011 AGY coding worker | DONE | Live AGY native edit and registered Go test verified against a synthetic artifact. [Evidence](evidence/m2a-acceptance.md) |
| G-012 OpenCode coding worker | DONE | Live anonymous OpenCode Free native edit and registered Go test verified against a synthetic artifact. [Evidence](evidence/m2a-acceptance.md) |
| G-013 AGY native sub-agent | BLOCKED | Signed 1.5.4 also reported an invocation error without a child conversation identity. [Evidence](milestones/m2b-agent-orchestration.md#alpha4-native-child-and-checkpoint-acceptance) |
| G-014 OpenCode native sub-agent | BLOCKED | Background correlation passed offline. The live Free child attempt received a new upstream 429. [Evidence](milestones/m2b-agent-orchestration.md#alpha4-native-child-and-checkpoint-acceptance) |
| G-015 AGY session resume | DONE | Two live AGY turns recalled the first-turn marker in the same owned Headless stream. [M2B evidence](milestones/m2b-agent-orchestration.md) |
| G-016 OpenCode session lifecycle | DONE | Two live OpenCode Free turns recalled the first-turn marker in the same owned session. [M2B evidence](milestones/m2b-agent-orchestration.md) |
| G-017 Checkpoint recovery | DONE | OpenCode to AGY recovery survived a restart and passed the registered signed-integer bounds test. [Evidence](milestones/m2b-agent-orchestration.md#alpha4-native-child-and-checkpoint-acceptance) |
| G-018 Independent verification | DONE | Both live directions verified. A hash-bound high-severity finding blocked the intentional defect. [Evidence](evidence/m2a-acceptance.md) |
| G-019 Automated task decomposition | DONE | A high-level synthetic Go objective produced a validated live AGY plan, scoped edit and passing registered test. [Evidence](evidence/public-beta-acceptance.md) |
| G-020 Adaptive model routing | DONE | Role evidence, quality-tier admission, and capacity-aware selection are verified for the supported scope. [G-034 evidence](evidence/g034-capacity-orchestration.md) |
| G-021 Quota reservations | DONE | Atomic AGY family reservations, bounded weekly pacing, and verified OpenCode cooldown scope passed acceptance. [G-034 evidence](evidence/g034-capacity-orchestration.md) |
| G-022 Controller strategy | DONE | Matched controller quota is used for continuity signals and actionable warnings. Missing windows stay unknown. [G-034 evidence](evidence/g034-capacity-orchestration.md) |
| G-023 Model quality profiles | DONE | Four scoped synthetic roles passed one live deterministic calibration. Broader comparative calibration remains deferred. [Evidence](evidence/g023-g035-alpha4.md) |
| G-024 Scheduler reliability | DONE | Durable graph expansion, failure fencing, cancellation, concurrency and recovery passed. [Evidence](evidence/public-beta-acceptance.md) |
| G-025 Multi-project policies | DONE | Two independent synthetic repositories executed with separate scopes, artifacts and project limits. [Evidence](evidence/public-beta-acceptance.md) |
| G-026 Packaging and developer experience | DONE | Two Intel archives matched byte for byte. The extracted package passed clean-install MCP, fixture and diagnostic checks. [Evidence](milestones/m2b-agent-orchestration.md#alpha4-regression-and-installation) |
| G-027 AGY ACP | DEFERRED | Evaluate as an experimental route using the same quota pool. |
| G-028 Daniiislo fallback | DEFERRED | Preserve the existing integration and verify safe fallback conditions. |
| G-029 Third local provider | DEFERRED | User approval and live local inference evidence are required. |
| G-030 Copilot Student eligibility | DEFERRED | Keep disabled until Claude-only eligibility and cost policy are verified. |
| G-031 Multi-client IPC | DEFERRED | Evaluate after single-client acceptance. |
| G-032 Observability and dashboard | DEFERRED | Complete per-job evidence before adding an optional interface. |
| G-033 Open-source readiness | DONE | Apache-2.0, English text, public repository metadata, publication inventory and Intel macOS package verified. [Evidence](evidence/m2a-acceptance.md#open-source-readiness) |
| G-034 Adaptive quality and capacity orchestration | DONE | Three-source capacity, quality-first routing, reserve enforcement and MCP warnings passed deterministic and read-only live acceptance. [Evidence](evidence/g034-capacity-orchestration.md) |
| G-035 AGY 1.5.4 worker compatibility | DONE | Exact signed version passed scoped Headless read-only and opt-in trusted-local coding acceptance. [Evidence](evidence/g023-g035-alpha4.md) |
| G-036 OpenCode Free availability reconciliation | DONE | Exact-route zero-cost success superseded the original cooldown and survived restart. Later provider failures remain independent observations. [Evidence](evidence/g036-opencode-reconciliation.md) |

## M1 readiness evidence

- `OFFLINE_CORE_READY` was verified using a separate fixture state. The live state reported `FIXTURE_BATCH_NOT_VERIFIED`.
- `AGY_LIVE_READY` was verified by a live AGY job and independent inspection.
- `OPENCODE_LIVE_READY` was verified by the native GLM smoke.
- `MULTI_PROVIDER_LIVE_READY` was verified by the real AGY diff and OpenCode receiver in a single batch.

The M1 baseline passed Go tests, vet, the race suite, and the Intel macOS build. The acceptance verifier matched the original batch request, artifact and handoff hashes, and terminal evidence. [M1 acceptance](evidence/m1-build.md) · [OpenCode native transport](evidence/native-opencode.md)
