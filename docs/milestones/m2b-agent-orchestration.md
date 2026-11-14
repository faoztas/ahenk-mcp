# M2B — Native agent orchestration

The `0.3.0-alpha.2` baseline added a planned durable checkpoint and a pinned OpenCode Free foreground child profile. Native child and full live recovery acceptance were still open at that baseline.

## Accepted scope

- G-015 passed two sequential live AGY turns in one owned Headless stream. The second turn recovered a synthetic marker introduced only in the first turn. The attempt verified both terminal events and cumulative usage.
- G-016 passed the same recall test over one live OpenCode Free session. Ahenk pins the first turn's messages before sending the follow-up and rejects changed prior history. Sessions exist only within an attempt. Archived session IDs and provider IDs supplied by callers are outside this scope.
- G-017 verifies source identity, snapshot, digest, preimages, and write scope before restoring to a fresh private worktree. The optional `checkpoint_objective` persists the first stage and pauses the job. An explicit resume retains the original deadline and requires another provider family. Offline coverage includes artifact tampering, preimage tampering, source drift, and retry history.
- G-020 uses verified inventory only for fresh, synthetic read-only routes. Coding and independent-review admission retain their existing stronger evidence requirements.
- G-021 reserves one in-flight AGY attempt per shared quota family under the scheduler lock. OpenCode keeps the existing exact model reservations and applies explicit shared provider gates.

## Native-child acceptance

G-013 is BLOCKED (initially recorded as PARTIAL at Alpha.2). Signed AGY 1.5.3 on `gemini-3.8-flash-medium` emitted a native `invoke_subagent` step in state `ERROR`. The step carried child metadata without a conversation ID, tool result, or reported denial. The same outcome occurred with a temporary private Git workspace. The exact upstream cause remains unknown. Ahenk now distinguishes reported denial, invalid configuration, unsupported invocation, timeout, and opaque invocation failure. A successful attempt requires an owned child terminal result whose digest matches the parent summary. [AGY Headless protocol](https://antigravity.google/docs/cli/headless/) · [AGY subagents](https://antigravity.google/docs/subagents?tab=cli)

G-014 is BLOCKED on live quota. The installed OpenCode 2.2.6 API returned the expected native child agent and effective permissions. The experimental foreground route requires the native `subagent` tool's completion metadata, matching child `parentID`, pinned model, workspace and permissions, and the child's independent terminal message with zero reported cost. Source review also found the distinct background notification, which includes a provider-owned child ID and terminal status. This profile uses foreground execution and owns the parent, child, and private process lifetime. The verified Free launch returned HTTP 429 on `opencode/glm-5.3-flash-free`. The provider-reported cooldown ended at approximately 2026-11-11 00:00 UTC. No child result was obtained and no alternate model was used. [OpenCode v2.2.6 native tool](https://github.com/anomalyco/opencode/blob/v2.2.6/packages/core/src/tool/plugin/subagent.ts) · [Background completion source](https://github.com/anomalyco/opencode/blob/v2.2.6/packages/core/src/session/subagent-completion.ts)

## Live checkpoint recovery

A synthetic Go job paused after AGY `gemini-3.8-flash-medium` used native read and edit tools to normalize negative inputs. The provider terminated, the private worktree was released, and the verified artifact and attempt were durably recorded. The acceptance harness stopped the Ahenk owner after the pause, reopened the same SQLite state, confirmed the unchanged checkpoint, and called `resume_job`. OpenCode `opencode/glm-5.3-flash-free` restored the checkpoint into a fresh worktree, added an upper cap, reported zero cost, and passed the registered `TestWholeDollar` with exit status 0. Both persisted diff hashes and the preimage matched. The source checkout and Git index were unchanged. The final diff changed only `calc.go` and contained a single sign normalization.

| Evidence | SHA-256 |
| --- | --- |
| Original `calc.go` preimage | `6ff978fdb76a026f56b21c3f794116377a89480f0d31d46a96469bf43cbb37cc` |
| AGY checkpoint diff | `d506d6d196770eb5d410fec309494c98534c4bc4c6bd54cbb0398d34fa346fc9` |
| OpenCode recovered diff | `eef8c0528b08e29b13a4775a9ee34028f61cddea35f789ca52f883ff1abebe1c` |

An independent offline test of that exact recovered diff failed on `math.MinInt`. Negation overflow returned the original negative value instead of the cap. At Alpha.2, G-017 was PARTIAL because the initial live artifact failed this boundary case; G-017 subsequently reached DONE in Alpha.4 after rerunning with the signed-integer boundary test (see [Alpha.4 native child and checkpoint acceptance](#alpha4-native-child-and-checkpoint-acceptance) below).

## Live continuation evidence

| Provider and model | Turns | Result | Session identity SHA-256 |
| --- | ---: | --- | --- |
| AGY `gemini-3.8-flash-medium` | 2 | Exact second-turn recall | `8ba750b69b4791c82d493fe2edf8bf38f1391a5db1e8169b272cab6366e7f02a` |
| OpenCode Free `opencode/glm-5.3-flash-free` | 2 | Exact second-turn recall | `d9a785a7593c1f2d75eb1960ecc7455918eeec3db16acdf850a0f9d6ea440ef4` |

Both jobs verified an unchanged synthetic source and index. The model audit and continuation used existing subscription and anonymous Free routes. [Model audit](../evidence/model-compatibility-2026-11-10.md) · [M2A baseline](../evidence/m2a-acceptance.md)

## Follow-on work

At Alpha.2, G-019, G-022, G-024 and G-025 were deferred. G-023 had an initial role evidence inventory. G-026 had SemVer reporting and the Intel package path. The verified readiness was `M2A_TRUSTED_LOCAL_CODING_REVIEW_READY`.

## Regression and local package

For `0.3.0-alpha.1`, the complete Go suite, race suite, vet and module verification passed. Coverage includes the eight-tool MCP contract, M1 fixture workflow, SQLite compatibility, M2A coding and review contracts, and M2B routing and checkpoint tests. The Intel macOS package built with CGO disabled, reported `0.3.0-alpha.1`, and contained the byte-identical Apache-2.0 license and dependency attributions. An offline stdio smoke test of the extracted binary discovered all eight tools and 20 additional audited read-only routes. The publishable-file audit found no private runtime files in the candidate inventory.

For `0.3.0-alpha.2`, the complete Go suite, race suite, vet and module verification passed. The CGO-disabled Intel build produced an x86-64 Mach-O executable. The extracted package reported `0.3.0-alpha.2` and exposed all eight MCP tools over stdio. Its Apache-2.0 license matched the source byte for byte, and dependency attributions were present. The regression suite covered SQLite state, M1 handoff and M2A contracts. The `math.MinInt` failure concerns the separate live recovery artifact described above.

## G-034 capacity orchestration

Alpha.3 adds controller-aware quota protection and quality-based selection without changing the native-child or live checkpoint acceptance gates. The highest verified coding readiness remains `M2A_TRUSTED_LOCAL_CODING_REVIEW_READY`. See the [G-034 acceptance record](../evidence/g034-capacity-orchestration.md) for the normalized live observations, deterministic scenarios, and MCP examples.

## Alpha.4 native child and checkpoint acceptance

G-036 reconciled the old GLM gate after one verified zero-cost native success and retained the recovery after restart. A later child attempt produced a fresh 429. G-023 qualified four bounded synthetic roles in a single signed AGY 1.5.4 read-only attempt. G-035 also verified opt-in trusted-local writes through the checkpoint acceptance below. [OpenCode root cause and recovery](../evidence/g036-opencode-reconciliation.md) · [AGY role and compatibility evidence](../evidence/g023-g035-alpha4.md)

- **G-013 BLOCKED.** On signed AGY 1.5.4, the native invocation reached `invoke_subagent` and emitted `ERROR` with child metadata but no conversation ID, tool output, or provider error detail. The synthetic source and index were unchanged. The adapter retained the separate 1.5.4 native-child gate after this diagnostic. [AGY Headless protocol](https://antigravity.google/docs/cli/headless/)
- **G-014 BLOCKED.** The experimental `native_child_background` OpenCode route requires the native launch metadata, parent-linked child session, provider-owned completion notification, and independently retrieved terminal result. Offline fixtures covered successful correlation, launch without completion, and an unrelated child identity. The single live attempt passed the local guard, sent the parent prompt, and received a fresh upstream 429 before an owned child result. Source and index stayed unchanged. [OpenCode 2.2.6 background notification](https://github.com/anomalyco/opencode/blob/v2.2.6/packages/core/src/session/subagent-completion.ts)
- **G-017 DONE.** OpenCode GLM created the scoped sign-normalization checkpoint and reported zero cost. Ahenk persisted the diff and preimage and confirmed the worker worktree was released. The owner was stopped after that durable pause. The restarted scheduler reassigned the job to AGY 1.5.4, verified the checkpoint before replay, and validated the final diff and actual registered test exit status. The added boundary fixture rejected the earlier overflow offline. The live `TestWholeDollar` passed with `math.MinInt`, `math.MaxInt`, negative and positive caps, and ordinary values. Only `calc.go` changed in the artifact, with one sign normalization. The source checkout and Git index remained unchanged.

| Checkpoint evidence | SHA-256 |
| --- | --- |
| Original preimage | `6ff978fdb76a026f56b21c3f794116377a89480f0d31d46a96469bf43cbb37cc` |
| OpenCode partial diff | `d506d6d196770eb5d410fec309494c98534c4bc4c6bd54cbb0398d34fa346fc9` |
| AGY final diff | `385580c7659f00165d713a731e8c57133880700f68088713d10457d0faeb5080` |

Highest verified readiness is `M2B_SESSION_RECOVERY_READY`. Full native subagent and autonomous orchestration acceptance remain open.

## Alpha.4 regression and installation

Full Go tests, race checks, vet, and module verification passed. Coverage includes the eight MCP tools, old SQLite records, M1 handoff, M2A coding and review contracts, G-034 quality and quota policy, G-036 reconciliation, four role fixtures, AGY startup and cancellation, and native-child and checkpoint cases.

Two CGO-disabled Intel macOS builds produced byte-identical archives. SHA-256 is `08e1b313325bc9fdfe781dc229dec0b0ceeda947d4ae7ab9995385945d26717e`. The extracted binary reported `0.3.0-alpha.4 darwin/amd64`, exposed eight MCP tools, completed an offline fixture in a fresh state directory, and returned valid read-only readiness diagnostics. The package contained the byte-identical Apache-2.0 license and dependency attributions. G-026 is complete for this local Intel installation path.

## Public beta orchestration

G-019, G-024 and G-025 add bounded automatic planning, durable lifecycle transitions, and per-project admission and scheduling limits. The existing eight MCP tools and SQLite schema are preserved. A live synthetic coding DAG produced a verified Go diff and passed the registered test. Two synthetic project policies and restart and cancellation faults were verified offline.

The provider-native child profiles remain experimental. G-013 is blocked by the opaque signed AGY 1.5.4 invocation error and missing child evidence. G-014 is blocked by a fresh upstream OpenCode Free rate limit before child completion. See the [public beta acceptance record](../evidence/public-beta-acceptance.md) for separate core and native-agent decisions.
