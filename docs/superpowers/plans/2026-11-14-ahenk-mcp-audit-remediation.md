# Ahenk MCP Audit Remediation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `.agents/vendor/superpowers/skills/subagent-driven-development/SKILL.md` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remediate all 15 verified security, routing, concurrency, IPC lifecycle, schema, and reliability findings in `ahenk-mcp` using strict TDD, task-by-task independent code review, and full verification.

**Architecture:** Fix root causes inside existing package boundaries (`providers`, `orchestration`, `mcpserver`, `ipc`, `model`, `cmd/ahenk`) without breaking existing wire contracts or Daniiislo/fixture invariants. Unify MCP auto-routing with `EvaluateRouting`/operational-stage logic, isolate read-only route previews from `HALF_OPEN` probe locks, synchronize dynamic repo registration under `Engine.mu`, harden child CLI/IPC sandboxes, and fix IPC socket/proxy shutdown semantics.

**Tech Stack:** Go 1.27, SQLite (`modernc.org/sqlite`), MCP Go SDK (`github.com/modelcontextprotocol/go-sdk/mcp`), `go test -mod=readonly`, `CGO_ENABLED=1 go test -race -mod=readonly`.

---

## Global Constraints

1. **Provider verification:** Use offline fixtures for automated checks. Live provider verification uses the configured and approved routes.
2. **Publication:** Track per-task diffs under `.superpowers/sdd/` during implementation. Commit and push when explicitly requested, as specified in `AGENTS.md`.
3. **Strict TDD:** For every behavioral change or bug fix, write a failing test first (RED), verify the failure output, implement the minimal fix (GREEN), verify the pass output, and report both RED and GREEN evidence in the task report.
4. **Thread-safety & `-race` cleanliness:** All shared mutable state (`e.cfg.Repositories`, `CircuitBreaker`, `HarnessServer`, `Daemon`) must be concurrency-safe under `go test -race -mod=readonly ./...`.
5. **Backwards compatibility:** Preserve all existing MCP tool names, CLI flags, `Submission`/`DelegateTaskInput` wire formats, `EvaluateRouting` signature, and existing test contracts (including `TestStdioSnapshotAndLifecycle` concurrent proxy clients).

## Review Focus

- **Security:** Recursion guard (`AHENK_MAX_DEPTH` clamp, `AHENK_CHILD_DEPTH` propagation), secret env suffix stripping (`*_API_KEY`, `*_TOKEN`, `*_SECRET`, `*_PASSWORD`), `os.Root` / `s.mu` confinement in `HarnessServer`, `PrivateSource` risk classification ordering before `CheckRequest`.
- **Correctness & State:** Zero `HALF_OPEN` probe lock leaks on `previewRoute`, `Capacity`, `JobCapacityWarnings`, early `run()` exits, or normalized provider keys; `rememberRateLimit` remaining-duration replay; `Engine.EvaluateAutoRoute` wiring and audit status synchronization.
- **IPC & Shutdown:** `Daemon.Close()` removing `SocketPath(stateDir)` and closing active conns; `RunProxy` cancelling cleanly without spurious failover on client stdin EOF; `HarnessServer.Close()` tracking connection goroutines via `wg`.

---

### Task 1: Provider Security & CLI Execution Hardening

**Files:**
- Modify: `internal/providers/recursion_guard.go`
- Modify: `internal/providers/recursion_guard_test.go`
- Modify: `internal/providers/codex_cli.go`
- Modify: `internal/providers/codex_cli_test.go`
- Modify: `internal/providers/claude_cli.go`
- Modify: `internal/providers/claude_cli_test.go`

- [ ] **Step 1 (RED):** Add failing tests in:
  - `internal/providers/recursion_guard_test.go`:
    1. `TestChildEnvSecretFiltering_Suffixes`: verify `MY_API_KEY`, `CUSTOM_TOKEN`, `APP_SECRET`, `DB_PASSWORD` are stripped while benign vars (`PATH`, `HOME`, `GOPATH`, `KEYBOARD_LAYOUT`) are kept.
    2. `TestCheckRecursionGuard_EnvMaxDepthCannotBeBypassed`: when `env` contains `AHENK_CHILD_SESSION=1`, `AHENK_CHILD_DEPTH=1`, `AHENK_MAX_DEPTH=1`, calling `CheckRecursionGuard(env, 99)` still denies (`POLICY_DENIED` / `RECURSIVE_DELEGATION_DENIED`); also test `DepthFromEnv` reading depth + clamped max depth.
  - `internal/providers/codex_cli_test.go`:
    1. Non-zero exit with non-empty `agent_message` summary returns an error (never drops `waitErr`).
    2. Exit 0 with empty stdout returns `INVALID_OUTPUT` (never `nil` empty success).
    3. Earlier `RATE_LIMIT` / `AUTH_REQUIRED` stream error is not overwritten by a later generic error event.
    4. Propagates `AHENK_CHILD_DEPTH` from parent env (`AHENK_CHILD_DEPTH=1`, `AHENK_MAX_DEPTH=3` -> child sees `AHENK_CHILD_DEPTH=2`, `AHENK_MAX_DEPTH=3`).
  - `internal/providers/claude_cli_test.go`:
    1. Non-zero exit with non-empty JSON `"result"` returns an error (never drops `waitErr`).
    2. Oversized stdout (>8 MiB) is bounded rather than unbounded buffering.
    3. Propagates `AHENK_CHILD_DEPTH` / clamped `AHENK_MAX_DEPTH`.
- [ ] **Step 2:** Run `go test -mod=readonly ./internal/providers` and confirm new tests fail (RED).
- [ ] **Step 3 (GREEN):** Implement fixes:
  - `internal/providers/recursion_guard.go`:
    - Update `isBlockedSecretEnv` to block exact `TOKEN`/`SECRET`/`PASSWORD`/`KEY`/`API_KEY`, existing prefixes, AND suffixes `_API_KEY`, `_KEY`, `_TOKEN`, `_SECRET`, `_PASSWORD` (preserving benign keys like `KEYBOARD_LAYOUT`).
    - Add `DepthFromEnv(env []string) (currentDepth, maxDepth int)` and `EffectiveMaxDepth(env []string, requestedMaxDepth int) int` helpers.
    - Update `CheckRecursionGuard(env []string, maxDepth int)` to clamp effective `maxDepth` using `AHENK_MAX_DEPTH` from `env` when positive, and fail closed if `AHENK_CHILD_SESSION=1` with invalid/non-positive depth or `current >= effectiveMaxDepth`.
    - Update `ChildEnv` to cap `maxDepth` if parent env has a lower positive `AHENK_MAX_DEPTH`.
  - `internal/providers/codex_cli.go`:
    - Use `DepthFromEnv(os.Environ())` when constructing `cmd.Env`.
    - Do not overwrite higher-priority `RATE_LIMIT`/`AUTH_REQUIRED` `streamErr` with lower-priority errors.
    - Never ignore `waitErr != nil` even if `evidence.Summary != ""`; classify `waitErr`/stderr (`RATE_LIMIT`, `AUTH_REQUIRED`, else `EXECUTION_FAILED`).
    - Return `INVALID_OUTPUT` if `waitErr == nil && streamErr == nil && evidence.Summary == ""`.
  - `internal/providers/claude_cli.go`:
    - Use `DepthFromEnv(os.Environ())` when constructing `cmd.Env`.
    - Bound `io.Copy(&stdoutBuf, io.LimitReader(stdoutPipe, (8<<20)+1))` and fail with `INVALID_OUTPUT` if stdout exceeds `8<<20`.
    - Never ignore `waitErr != nil` even if `evidence.Summary != ""`.
  - `internal/providers/claude_cli_test.go`: increase tight `5*time.Second` subprocess timeouts in `TestRunClaudeCLI_Success`, `TestRunClaudeCLI_RateLimitError`, `TestRunClaudeCLI_OverloadedError_HTTP529` (and sibling unit tests) to `15*time.Second` to eliminate `-race` parallel load flakiness.
- [ ] **Step 4 (VERIFY):** Run `CGO_ENABLED=1 go test -race -mod=readonly ./internal/providers` and verify all tests pass cleanly.

---

### Task 2: Provider Registry Speed, Multi-Entry Config, & TargetEffort Propagation

**Files:**
- Modify: `internal/providers/providers.go`
- Modify: `internal/providers/providers_test.go` (or `internal/providers/providers_cli_test.go`)
- Modify: `internal/providers/codex_cli.go` (effort sanitization)

- [ ] **Step 1 (RED):** Add failing tests in `internal/providers/providers_test.go` / `internal/providers/codex_cli_test.go`:
  1. `Estimate` + `Budget` with default `"read_only"` task (`120s` deadline) on `agy`, `opencode`, audited models, and custom configured `codex`/`claude` models without explicit `Speed`: verify `providers.New` assigns default `Speed: "fast"` when `p.Speed == ""` (except explicitly slow/fixture) so `Budget` admits default `read_only` tasks (`60s <= 100s`) instead of rejecting with `DEADLINE_BUDGET` (`180s > 100s`). Preserve explicit `"slow"` behavior (`Estimate` = `240s`).
  2. Multi-entry `codex` / `claude` entries in `Config.Providers`: configuring multiple custom models registers all of them cleanly instead of dropping subsequent entries.
  3. `sanitizeCodexEffort("max")` maps to `"xhigh"` (not `"medium"`).
  4. Verify `TargetEffort` handling in `BuildClaudeArgs` / `providers.go`: pass or map cleanly without breaking existing mock CLI scripts (`claude_cli_test.go`), documenting non-CLI providers cleanly.
- [ ] **Step 2:** Run `go test -mod=readonly ./internal/providers` and confirm RED failure.
- [ ] **Step 3 (GREEN):** Update `internal/providers/providers.go` and `internal/providers/codex_cli.go`:
  - Assign default `Speed: "fast"` in `providers.New` for `agy`, `opencode`, audited models, and custom `codex`/`claude` models whenever `Speed == ""` (while honoring explicit `p.Speed` or `row.LatencyMS`-derived slow hints if present).
  - Support multiple explicit `codex`/`claude` entries in `c.Providers` without overwriting/dropping earlier models.
  - Map `"max"` -> `"xhigh"` in `sanitizeCodexEffort`.
- [ ] **Step 4 (VERIFY):** Run `CGO_ENABLED=1 go test -race -mod=readonly ./internal/providers` and confirm PASS.

---

### Task 3: CircuitBreaker Normalization, `previewRoute` Read-Only Isolation, & Startup Rate-Limit Replay

**Files:**
- Modify: `internal/orchestration/circuit_breaker.go`
- Modify: `internal/orchestration/circuit_breaker_test.go`
- Modify: `internal/orchestration/capacity.go`
- Modify: `internal/orchestration/capacity_test.go`
- Modify: `internal/orchestration/engine.go`
- Modify: `internal/orchestration/outage_resiliency_test.go`

- [ ] **Step 1 (RED):** Add failing tests in `internal/orchestration/circuit_breaker_test.go` and `internal/orchestration/outage_resiliency_test.go`:
  1. `TestPreviewRouteDoesNotLeakHalfOpenProbeLock`: put a provider in `HALF_OPEN` state (cooldown expired), call `Engine.Capacity`, `Engine.JobCapacityWarnings`, and `Engine.previewRoute`, and assert `e.CircuitBreaker().IsProbing(provider)` stays `false` and `Allow(provider)` still grants the real execution probe.
  2. `TestRememberRateLimit_UsesRemainingDurationFromAttemptFinishedAt`: replay an attempt whose `FinishedAt` was 10 minutes ago (with 1-minute cooldown) and verify `CircuitBreaker` is NOT re-tripped for another full minute at startup; replay a fresh attempt (`FinishedAt` 5s ago, 60s delay) and verify cooldown expires at `FinishedAt + 60s`.
  3. `TestRoute_MultiModelProviderAndEarlyReturnReleasesProbe`: test `allowedProviders` keying by `normalizeProvider` (e.g. mixed `"antigravity"` / `"agy"` or multi-model `"codex"`) and verify `e.cb` probe lock is released on every failure / early-exit path in `run()`.
  4. `TestOpenCodeRateLimitUpdatesCircuitState`: when `opencode` hits `RATE_LIMIT` with pool/account scope (or repeated failures), ensure circuit state / cooldown is reflected consistently and no dangling probe lock remains.
- [ ] **Step 2:** Run `go test -mod=readonly ./internal/orchestration` and confirm RED failures.
- [ ] **Step 3 (GREEN):** Implement:
  - `circuit_breaker.go`: add `TripUntil(provider string, until time.Time)` (no-op if `!until.After(time.Now())`), ensure every method uses `normalizeProvider(provider)`, and keep `ActiveTripped()` clean.
  - `engine.go`:
    - Factor `e.routeWithMode(j *model.Job, now time.Time, dryRun bool)` (keep `e.route(j, now)` delegating with `dryRun: false`).
    - When `dryRun == true`, check `e.cb.Eligible(c.Provider)` and NEVER call `e.cb.Allow` or `e.cb.Release`.
    - Key `allowedProviders` by `normalizeProvider(c.Provider)` in all check/release sites inside `routeWithMode`.
    - Release acquired probe lock on `a.Code == reserveReason` and `a.Checkpoint != nil` early returns in `run()` (in addition to `execution.NewAttemptDir` / `e.fatal` / `finish()`).
    - Fix `rememberRateLimit` to pass `until = attempt.FinishedAt.Add(delay)` via `e.cb.TripUntil(attempt.Provider, until)` (ignoring already-expired historical limits).
    - Ensure `opencode` `RATE_LIMIT` updates `e.cb` appropriately for pool/provider-level rate limits while per-model cooldowns remain model-scoped and release probe locks cleanly.
  - `capacity.go`: have `previewRoute` call `e.routeWithMode(&j, now, true)`.
- [ ] **Step 4 (VERIFY):** Run `CGO_ENABLED=1 go test -race -mod=readonly ./internal/orchestration` and confirm PASS.

---

### Task 4: Unified Auto-Routing Engine (`EvaluateRouting` Alignment, Quota Freshness, & Audit Sync)

**Files:**
- Modify: `internal/orchestration/routing.go`
- Modify: `internal/orchestration/quota.go`
- Modify: `internal/orchestration/engine.go`
- Modify: `internal/store/routing_audit.go`
- Modify: `internal/orchestration/multi_provider_routing_e2e_test.go`

- [ ] **Step 1 (RED):** Add failing tests:
  - `internal/orchestration/quota.go`: `HealthPercent` with a stale/expired quota row returns `1.0` (unobserved/stale default after `q.At(now)`) rather than the expired `Remaining`, and `HealthPercent(providers.BarAgyArgon)` reads `"gemini-argon-wtu-weekly"` accurately.
  - `internal/orchestration/multi_provider_routing_e2e_test.go`:
    1. `EvaluateAutoRoute` honors `RepoID` allowed providers, candidate-level executability (`patch` vs `read_only`, `TargetTier`, `DEADLINE_BUDGET`), `gemini_argon` vs `claude_gpt` vs `gemini` health bars, explicit `complexity == 0.0` (when specified), unknown category deterministic fallback, `DetermineOperationalStage` throttling, and `db.SaveQuotaState` persistence.
    2. Audit lifecycle test: `Engine.persist` updates `job_routing_audit` to `SUCCESS` only on `model.Completed` and `FAILED`/`CANCELLED`/`TIMED_OUT` -> `FAILED` without `mcpserver` racing and overwriting it.
- [ ] **Step 2:** Run `go test -mod=readonly ./internal/orchestration ./internal/store` and confirm RED failure.
- [ ] **Step 3 (GREEN):** Implement:
  - `quota.go`: apply `q = q.At(now)` inside `HealthPercent` (taking `now := time.Now().UTC()` once under `e.mu`) and support `BarAgyArgon` (`"gemini-argon-wtu-weekly"`).
  - `routing.go`: preserve `EvaluateRouting` signature/behavior for existing callers and add `(e *Engine) EvaluateAutoRoute(repoID, category string, complexity *float64, task model.Task, now time.Time) (RouteDecision, *model.Capability)` that:
    - Uses `FallbackMatrix(normalizedCat)` with deterministic `"coding"` fallback if category is unrecognized.
    - Distinguishes omitted complexity (`nil`) vs explicit `0.0`.
    - Filters candidates by repo allowlist (under `e.mu`), candidate capability availability & mode/profile admissibility (`AllowsTrustedLocal`, `PrivateSource`, `patch` CLI support), `CircuitBreaker.Eligible`, `cooldown`/`openCodeGate`, model-family quota health (`BarAgyGemini`, `BarAgyOpus`, `BarAgyArgon`), `TargetTier`, `DEADLINE_BUDGET`, and `ValidateModelAdmission`.
    - Computes `DetermineOperationalStage`, calls `ResolveEffort` + stage throttle, and persists `db.SaveQuotaState` cleanly.
  - `engine.go` & `routing_audit.go`: ensure `Engine` owns/synchronizes audit status updates cleanly so terminal status transitions (`Completed` -> `SUCCESS`, everything else terminal -> `FAILED`) never overwrite or race with synchronous wait callers.
- [ ] **Step 4 (VERIFY):** Run `CGO_ENABLED=1 go test -race -mod=readonly ./internal/orchestration ./internal/store` and confirm PASS.

---

### Task 5: Dynamic Repo Locking, `DelegateTaskInput` Schema/Normalization, & MCP Tool Handlers

**Files:**
- Modify: `internal/model/model.go`
- Modify: `internal/model/model_test.go` (create if missing)
- Modify: `internal/orchestration/engine.go`
- Modify: `internal/mcpserver/server.go`
- Modify: `internal/mcpserver/server_test.go` (create/expand)

- [ ] **Step 1 (RED):** Write failing tests:
  - `internal/model/model_test.go`:
    1. `DelegateTaskInput.UnmarshalJSON` returns an error on malformed `"task"` or `"target"` JSON, preserves `Complexity: 0.0` explicitly vs omitted, preserves extra `Task` fields (`CheckpointObjective`, `FollowUp`, `RoleCalibration`, `OpenCodeRecheck`, `AGYReserveOverride`, `NativeChild`, `NativeChildBackground`, `TaskClass`, `Risk`).
    2. `WaitJobsInput.Normalize` deduplicates `JobIDs` preserving order and rejects oversized lists (>32).
  - `internal/mcpserver/server_test.go`:
    1. `TestHandleDelegateTask_DynamicRepoAndMaxDepth`: dynamically registered repo via `ResolveRepo(cwd)` is immediately visible to `list_capabilities` / `handleDelegateTask` even though startup `cfg` had 0 repos; `AHENK_MAX_DEPTH=1` in env blocks recursive delegation even when client sends `max_depth: 10`; whitespace-prefixed JSON args succeed in `add()`.
    2. `TestGetJobResult_DiffDefaultArtifactAndNilResult`: `get_job_result(view: "diff")` without `artifact_id` uses `j.Result.ArtifactID`, and incomplete/no-result jobs return a structured error rather than nil-pointer panicking.
    3. `TestDiagnosticTools_QuotasBreakersAuditWaitJobs`:
       - `get_provider_quotas` reports `UNKNOWN` (not fake `HEALTHY`) when `e == nil`, and includes `gemini-argon-wtu-weekly`.
       - `get_active_circuit_breakers` returns deduplicated entries with a single consistent schema (`provider`, `model`, `scope`, `code`, `state`, `until`).
       - `routingSummary` returns `[]` (not `null`) when no breakers are active and skips providers with open circuit breakers or `0%` health (`codex`/`claude`).
       - `get_routing_audit` validates `ValidID(in.JobID)` and enforces `repoAllowed` (`NOT_FOUND` on cross-repo/unknown job IDs).
       - `WaitJobs` checks `e.repoAllowed` on requested `job_ids`; `list_jobs`, `get_job_status`, `cancel_job`, `resume_job` validate IDs cleanly.
- [ ] **Step 2:** Run `go test -mod=readonly ./internal/model ./internal/orchestration ./internal/mcpserver` and confirm RED failure.
- [ ] **Step 3 (GREEN):** Implement:
  - `model.go`: track `HasComplexity bool` (json `"-"` populated during `UnmarshalJSON` whenever `"complexity"` key appears in raw payload or nested task), propagate missing `Task` fields in `UnmarshalJSON`/`Normalize`, return unmarshal errors for invalid non-null `RawTask`/`RawTarget`, deduplicate/cap `WaitJobsInput`.
  - `engine.go`: lock `e.mu` around `e.cfg.Repositories` reads in `Submit`, `submitBatch`, and add mutex-safe accessors `Engine.Repo(id string)`, `Engine.RepoProviders(id string)`, `Engine.HasRepo(id string)`, `Engine.LocalRunners()`. Enforce `e.repoAllowed` inside `WaitJobs`.
  - `mcpserver/server.go`:
    - Use `Engine` accessors instead of stale `cfg` value copy.
    - Replace inline duplicated auto-routing in `handleDelegateTask` with `e.EvaluateAutoRoute(...)` + `providers.EffectiveMaxDepth(os.Environ(), in.MaxDepth)`.
    - Remove `mcpserver` race calling `UpdateRoutingAuditStatus` with wrong status (let `Engine` own terminal status transition, recording initial `"DISPATCHED"` safely before terminal wait or inside `Engine`).
    - Fix `get_job_result(view: "diff")` default `in.ArtifactID` & `j.Result != nil` guard.
    - Fix `get_provider_quotas`, `get_active_circuit_breakers`, `routingSummary`, `get_routing_audit`, `get_job_status`, `list_jobs`, `cancel_job`, `resume_job`.
    - In `add()`: `bytes.TrimSpace(args)` before `args[0] != '{'`; remove unused `closeObjects` dead helper or apply safely only if compatible with dual string/object schemas (`DelegateTaskInput`).
- [ ] **Step 4 (VERIFY):** Run `CGO_ENABLED=1 go test -race -mod=readonly ./internal/model ./internal/orchestration ./internal/mcpserver` and confirm PASS.

---

### Task 6: IPC Daemon Socket Cleanup, Proxy Failover/Shutdown, & Harness Sandbox/Lifecycle

**Files:**
- Modify: `internal/ipc/daemon.go`
- Modify: `internal/ipc/proxy.go`
- Modify: `internal/ipc/harness.go`
- Create/Modify: `internal/ipc/daemon_test.go`
- Modify: `internal/ipc/harness_test.go`
- Modify: `cmd/ahenk/main.go`
- Modify: `cmd/ahenk/main_test.go`

- [ ] **Step 1 (RED):** Add failing tests:
  - `internal/ipc/daemon_test.go`:
    1. `TestDaemonCloseRemovesAbsoluteSocketPath`: start daemon in temp `stateDir`, connect a client, call `d.Close()` from a *different* working directory, verify `SocketPath(stateDir)` is removed (`os.Stat` returns `ErrNotExist`) and active client connections terminate promptly without hanging `d.wg.Wait()`.
    2. `TestRunProxy_ContextCancelDoesNotHang`: start a silent server socket + blocked `io.Pipe` stdin, cancel `ctx`, verify `RunProxy` returns promptly and does NOT call `failover`.
    3. `TestRunProxy_ClientStdinEOFDoesNotTriggerFailover`: send EOF on client stdin and verify server responses flush and `RunProxy` returns `nil` without calling `failover`.
    4. `TestRunProxy_UnexpectedLeaderDropTriggersFailover`: server drops connection while stdin pipe stays open; verify `failover` is invoked cleanly.
  - `internal/ipc/harness_test.go`:
    1. `TestHarnessServerCloseWaitsForAndClosesConnections`: open an idle client connection, call `s.Close()`, verify `Close()` closes active conns and waits for goroutines cleanly under `-race`.
    2. `TestHarnessServer_CheckRecursionRespectsConfigEnv`: `HarnessServer{cfg: {Env: ["AHENK_CHILD_SESSION=1", "AHENK_CHILD_DEPTH=1", "AHENK_MAX_DEPTH=1"]}}` denies `CheckRecursion(nil)`.
    3. `TestHarnessServer_ReadFileSizeLimit`: reading a >4 MiB file returns a bounded error.
    4. `TestHarnessServer_JSONRPCErrorShape`: JSON-RPC error response includes structured `error: {code, message}`.
  - `cmd/ahenk/main_test.go`:
    1. Verify leader `serve --stdio` while proxy clients are connected honors shutdown cleanly (test leader waiting for active proxy clients or failover behavior cleanly without hanging existing `TestStdioSnapshotAndLifecycle`).
- [ ] **Step 2:** Run `go test -mod=readonly ./internal/ipc ./cmd/ahenk` and confirm RED failures.
- [ ] **Step 3 (GREEN):** Implement:
  - `ipc/daemon.go`: store `sockPath` on `Daemon`; track active client `net.Conn`s (`map[net.Conn]struct{}` protected by `mu`), close them on `Close()`, remove `d.sockPath`, share single `chdirMu` with `harness.go`, and check `os.Getwd()` / `os.Chdir()` errors.
  - `ipc/proxy.go`: fix `RunProxy` to track `stdinEOF` atomic bool, close `conn` on `ctx.Done()`, unblock stdin read when `conn` closes (`SetReadDeadline`/done pipe/select), only run `failover` on unexpected leader loss while `!stdinEOF.Load() && ctx.Err() == nil`.
  - `ipc/harness.go`: track `handleConn` in `s.wg` and active `conns` map on `Close()`; serialize `EditFile` with `s.mu` and use `os.OpenRoot` (`filepath.Dir`/worktree root) for symlink-safe confined I/O; bound `ReadFile` to `4<<20` bytes; fix `CheckRecursion` to include `s.cfg.Env`; populate `jsonRPCResponse.Error` properly for JSON-RPC callers; remove `chdirHarnessMu` duplicate in favor of shared `chdirMu`.
  - `cmd/ahenk/main.go`: check `serve` leader cleanup vs active IPC clients carefully so leader exits cleanly when stdin closes and no proxy clients remain (or drains active clients with bounded grace before shutdown so proxies fail over cleanly).
- [ ] **Step 4 (VERIFY):** Run `CGO_ENABLED=1 go test -race -mod=readonly ./internal/ipc ./cmd/ahenk` and confirm PASS.

---

### Task 7: Execution, PrivateSource, AGY Policy & CLI Edge Cases + Full Verification

**Files:**
- Modify: `internal/model/model.go`
- Modify: `internal/model/private_source.go`
- Modify: `internal/model/private_source_test.go` (or `model_test.go`)
- Modify: `internal/providers/agy.go`
- Modify: `internal/providers/agy_test.go`
- Modify: `internal/orchestration/implementation.go`
- Modify: `internal/orchestration/engine.go`
- Modify: `cmd/ahenk/local_test_command.go`
- Modify: `cmd/ahenk/private_source.go`
- Modify: `cmd/ahenk/main.go`
- Modify: `cmd/ahenk/main_test.go`

- [ ] **Step 1 (RED):** Add failing tests for:
  1. `model.Task.Validate` with `PrivateSource` + `"security"` in objective: must reject as `critical`/unsupported before passing `CheckRequest` (prevents security-classification bypass).
  2. `PrivateRoute.AccountMatches`: case-insensitive email match (`Alice@Example.com` vs `alice@example.com`) and hash match.
  3. `AllowsAGYProfile`: a task with `Mode: "patch"` cannot silently use `"restricted"` read-only profile.
  4. `runFixtureImplementation` / `runPrivateWriteImplementation`: primary error is preserved via `errors.Join(err, cleanupErr)` if `w.Close()` also fails; `runSubagentImplementation` passes `handoffs` through; `patch`+`Acceptance` cannot skip Go checks.
  5. `local-test` error text says `18 minutes`; `private-brief-digest` rejects invalid check IDs or secret/PII briefs; `probe-agy` iterates all `"agy"` entries; `run` usage message accurately lists commands; `serve --config` relative/abs handling matches documented behavior.
- [ ] **Step 2:** Run `go test -mod=readonly ./internal/model ./internal/providers ./internal/orchestration ./cmd/ahenk` and confirm RED failure.
- [ ] **Step 3 (GREEN):** Implement all fixes cleanly.
- [ ] **Step 4 (FINAL VERIFICATION):**
  - Run `go test -mod=readonly ./...`
  - Run `CGO_ENABLED=1 go test -mod=readonly -race ./...`
  - Run `go vet -mod=readonly ./...`
  - Run `make dist` (verify build artifacts, then remove `dist/`)
  - Re-run live stdio/IPC smoke test (`live_e2e_audit.py`) against newly compiled binary and confirm `sock_leftover: False`, `get_job_result(view: "diff")` without `artifact_id` succeeds, and all tools work cleanly; remove `/tmp/ahenk-audit` afterwards.

## Follow-up after the interrupted verification

Use OpenCode for implementation and independent review. The agents need the
existing uncommitted changes, so use the shared checkout with disjoint file
ownership. Run the follow-up regressions before rerunning the full checks.

- [x] Give the audit lifecycle fixture enough time to reach the scheduler's
  `NOT_RUN` decision. Verify that terminal jobs close out the dispatch row
  after retries and that duplicate audit inserts preserve the original decision.
- [x] Exercise the CLI when the IPC leader disappears. Verify that the
  disconnected proxy exits with a reconnect error and that a fresh MCP session
  can read existing jobs. Verify normal EOF, response flush and cancellation.
- [x] Compare the advertised `delegate_task` and `spawn_subagent` schemas with
  the task and target inputs the decoder accepts. Verify error responses retain
  the safe job metadata that callers need.
- [x] Verify that provider aliases used in route selection agree with the
  execution allowlist. Keep private route grants bound to their configured
  identities.
- [x] Apply the same price/performance tier filter to auto-routing and execution.
  Verify that an eligible OpenCode route can execute with the `free` target.
- [x] Verify that dynamic registration cannot mutate the caller's static
  configuration. Runtime registration is scoped to the engine instance.
- [x] Verify the original 18-minute local-test limit, public command usage, and
  per-entry AGY probe handling with offline CLI tests.
- [x] Diagnose the mock CLI deadline failures observed during the full suite.
  Verify the provider tests under the race detector and check ACP references
  before removing any unused code.
- [x] Verify archive contents and run the stdio and IPC smoke test against its
  binary. Check tool discovery, async dispatch, waits, verified diffs and
  socket cleanup.
- [x] Review routing, provider policy, job audit, MCP schemas and the harness
  independently. Verify the CLI reconnect behavior in separate process tests.

The final worktree passed the full `go test -mod=readonly ./...` and
`CGO_ENABLED=1 go test -mod=readonly -race ./...` runs across all nine packages.
`go vet -mod=readonly ./...`, `go mod verify`, and `git diff --check` passed.
The Intel macOS archive passed license and executable checks. Its binary
advertised 13 MCP tools and returned the verified diff without an artifact ID.

The independent OpenCode review found no confirmed regressions in its scope.
Separate process tests verified the final proxy changes. The leader crash
regression confirmed that a fresh session can read the persisted job.
A provider verification run overlapped the expected failing phase of the MCP
schema regression. The final schema passed 50 repetitions with the race detector.
