# M2A — Real coding agents

Codex App defines the objective, scope, risk and acceptance criteria. Ahenk selects the provider and model, manages the job and workspace, and returns verified results. Codex App retains the integration decision. The existing eight MCP tools and M1 patch handoff remain supported.

| Goal | Scope and acceptance |
| --- | --- |
| G-009 AGY profiles | Preserve the restricted pilot. Give analysis and review access to approved context. Enable implementation only with verified write and check permissions. Record advertised capabilities, observed calls, denials and terminal evidence separately. |
| G-010 Workspace isolation | Create one private detached worktree per attempt from a scoped immutable snapshot. Preserve staged, unstaged and explicitly selected untracked content as applicable. Verify source identity, source index, diff and file hashes. Test ownership, conflicting writers, unsafe paths and cleanup. |
| G-011 AGY implementation | A signed, pinned AGY route must modify a synthetic Go fixture inside the approved workspace, trigger a passing registered test and return a verified artifact. Use the existing subscription gate and request `useG1Credits:false`. |
| G-012 OpenCode implementation | An anonymous, zero-price OpenCode route must modify a separate synthetic Go fixture, trigger a passing registered test and return a verified artifact. Preserve provider cooldowns and bounded fallback. |
| G-018 Independent review | Verify AGY-to-OpenCode and OpenCode-to-AGY review with real artifacts and a defect-detection case. Tie the verdict to the verified diff. Require successful deterministic checks and distinct-provider review for critical implementation work. |
| G-033 Open-source readiness | Verify public repository metadata, Apache-2.0, upstream attribution, English maintained text, safe publication inventory and an Intel macOS build. |

## Implemented paths and gates

The explicit AGY `analysis` and `review` profiles receive scoped synthetic snapshot text through the audited restricted agent. That route keeps native filesystem and command tools disabled. The `implementation` profile has an offline fixture route only. It requires a fixture-enabled server, an approved repository and an explicit fixture preference. The fixture patch is applied inside a private worktree and independently verified. The default profile retains the direct-writer guard. Native writers use the separate `trusted-local` profile and require repository-level opt-in with named Go checks and exact write paths.

For artifact review, submit a synthetic `read_only` child with `execution_profile: "review"`, a direct patch dependency, a single `diff` handoff and batch `independence: "distinct_provider"`. The reviewer receives the source objective, acceptance criteria, scoped base context, changed paths, verification evidence and diff. Ahenk binds the verdict to the producer's artifact hash and attempt. A request for changes leaves the original artifact available and blocks the batch. The controller can submit a separate correction request. Ahenk starts no automatic correction loop.

A private worktree separates Git state. Trusted-local uses provider file permissions, checks the actual diff, then invokes the registered Go test independently. Checks must finish with a passing named test and zero exit status before handoff. Trusted-local subprocesses retain the local user's OS permissions. Strict host containment is tracked separately from trusted-local acceptance.

See the [M2A acceptance matrix](../evidence/m2a-acceptance.md) and [historical M1 evidence](../evidence/m1-build.md).
