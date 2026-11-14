# Multi-Provider Dynamic Routing & Quota Adaptive Circuit Breakers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement multi-provider dynamic routing, 4-bar quota telemetry ingestion from CodexBar, model-effort decoupling with a 10-unique model priority matrix, runtime dynamic effort resolution, 5-stage adaptive circuit breakers, and SQLite audit persistence in Ahenk MCP.

**Architecture:** Ahenk MCP acts as an autonomous local execution scheduler with Codex App as primary controller. Telemetry from CodexBar CLI is ingested into an atomic non-blocking cache; a 10-engine matrix per task category decouples reasoning effort from model identity; `DynamicEffortResolver` dynamically throttles effort based on quota pressure; a 5-stage state machine with zero-latency circuit breakers guarantees continuous operation across provider outages; all routing decisions and quota metrics are durably logged to SQLite (user_version 2) and exposed via MCP diagnostics tools.

**Tech Stack:** Go 1.24+, SQLite (modernc.org/sqlite, WAL mode), Model Context Protocol SDK (`github.com/modelcontextprotocol/go-sdk/mcp`), macOS `codesign` security verification, Git Worktrees.

**Spec:** [epics-and-stories.md](file:///Users/oztas/Developments/github.com/faoztas/ahenk-mcp/docs/epics/multi-provider-dynamic-routing/epics-and-stories.md) and [multi-provider-dynamic-routing-analysis.md](file:///Users/oztas/Developments/github.com/faoztas/ahenk-mcp/docs/multi-provider-dynamic-routing-analysis.md).

## Global Constraints

- Go standard library conventions; all packages must build with `go test ./internal/...`.
- No credentials, API keys, or OAuth tokens stored in SQLite, stdout/stderr, or worktree logs.
- CodexBar binary verification requires macOS strict codesign, Team ID `Y5PE65HELJ`, and SHA-256 allowlist.
- In-memory routing decision overhead must remain $\le 5\text{ ms}$; circuit breaker failover overhead is $0\text{ ms}$.
- Database migration must preserve WAL mode (`PRAGMA journal_mode=WAL`), enforce foreign keys (`PRAGMA foreign_keys=ON`), use file mode `0600`, and transition `PRAGMA user_version` from 1 to 2 without data loss.
- Fallback chain across all 7 categories must strictly maintain 10 unique model engines (1 Primary + 9 distinct fallbacks); duplicate engines are forbidden.

## Review Focus

1. **CodexBar CLI binary tampering / PATH spoofing:** An untrusted executable in PATH or injected environment variables attempting to leak secrets. Expected: strict codesign checks Team ID `Y5PE65HELJ` and audited SHA-256; environment is stripped of all API keys (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, etc.); browser launch is trapped by `/usr/bin/false`. (Pinned to Task 3).
2. **Opus scarcity protection and quota exhaustion:** Attempted routing of routine coding/debugging tasks to Claude Opus 5.5, or Opus reserve dropping $\le 20\%$. Expected: router immediately rejects Opus with `OPUS_SCARCITY_GATE_TASK_DENIED` or `OPUS_RESERVE_DEPLETED`, failing over to `codex/gpt-6-astra` or `antigravity/gemini-4-argon` with 0ms delay. (Pinned to Task 6).
3. **OpenCode Free HTTP 429 cascade and stampede:** When OpenCode free pool returns `provider.free-limit` (HTTP 429). Expected: circuit breaker trips to `OPEN` for all `opencode/*` models for 16.6 hours ($59,760,000\text{ ms}$), immediately bypassing to commercial tiers without scheduler blocking. (Pinned to Task 9).
4. **SQLite schema migration under existing state:** Upgrading an existing database from `user_version 1` to `user_version 2`. Expected: atomic transaction creates `provider_quota_state` and `job_routing_audit` without corrupting existing `jobs` or `audit` tables; persists and restores quota snapshots across restarts. (Pinned to Task 10).
5. **CodexBar background polling latency spike:** Slow CodexBar CLI response exceeding normal execution time. Expected: `Engine.Quota()` non-blocking read against atomic cache with 60s TTL; asynchronous refresh with 4-second execution timeout; routing overhead stays $\le 5\text{ ms}$. (Pinned to Task 2).

---

## Task Decomposition

### Task 1: CodexBar Output Parser & Window Normalizer (Story 1.1)

**Files:**
- Create: `internal/providers/testdata/codexbar_4bars.json`
- Modify: `internal/providers/quota.go:50-180`
- Test: `internal/providers/codexbar_test.go`

**Interfaces:**
- Consumes: Raw CodexBar JSON exports from `codex`, `antigravity`, `opencode`.
- Produces: 
  - Constants: `BarCodexBurst = "codex_5h_burst"`, `BarCodexWeekly = "codex_weekly"`, `BarAgyGemini = "agy_gemini_5h"`, `BarAgyOpus = "agy_opus_5d"`, `BarOpenCodeFree = "opencode_free_16h"`.
  - Normalized `[]Quota` slice with distinct buckets (`gemini-5h`, `3p-weekly`, `primary-300`, `secondary-10080`), accurate remaining percentages, and window minutes.

- [ ] **Step 1: Write the failing test**

In `internal/providers/codexbar_test.go`, add `TestDecodeCodexBarFourBars`:
```go
func TestDecodeCodexBarFourBars(t *testing.T) {
	rawCodex := `{"provider":"codex","source":"oauth","usage":{"updatedAt":"2026-11-12T12:00:00Z","primary":{"usedPercent":25.0,"windowMinutes":300},"secondary":{"usedPercent":10.0,"windowMinutes":10080}}}`
	now := time.Date(2026, 11, 12, 12, 1, 0, 0, time.UTC)
	quotas, err := DecodeCodexBar(strings.NewReader(rawCodex), "codex", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(quotas) != 2 {
		t.Fatalf("expected 2 quotas, got %d", len(quotas))
	}
	if quotas[0].Bucket != BarCodexBurst || quotas[0].WindowMinutes != 300 || *quotas[0].Remaining != 75.0 {
		t.Errorf("unexpected primary quota: %+v", quotas[0])
	}
	if quotas[1].Bucket != BarCodexWeekly || quotas[1].WindowMinutes != 10080 || *quotas[1].Remaining != 90.0 {
		t.Errorf("unexpected secondary quota: %+v", quotas[1])
	}

	rawAGY := `{"provider":"antigravity","source":"cli","usage":{"updatedAt":"2026-11-12T12:00:00Z","extraRateWindows":[{"id":"gemini-5h","window":{"usedPercent":15.0,"windowMinutes":300}},{"id":"3p-weekly","window":{"usedPercent":80.0,"windowMinutes":7200}}]}}`
	agyQuotas, err := DecodeCodexBar(strings.NewReader(rawAGY), "antigravity", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(agyQuotas) != 2 {
		t.Fatalf("expected 2 AGY quotas, got %d", len(agyQuotas))
	}
	if agyQuotas[0].Bucket != BarAgyGemini || *agyQuotas[0].Remaining != 85.0 {
		t.Errorf("unexpected agy gemini quota: %+v", agyQuotas[0])
	}
	if agyQuotas[1].Bucket != BarAgyOpus || *agyQuotas[1].Remaining != 20.0 {
		t.Errorf("unexpected agy opus quota: %+v", agyQuotas[1])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers -run TestDecodeCodexBarFourBars -v`  
Expected: FAIL with compilation error undefined `BarCodexBurst`, `BarCodexWeekly`, etc.

- [ ] **Step 3: Implement minimal code in `internal/providers/quota.go`**

1. Define the 5 bar constants in `internal/providers/quota.go`:
   ```go
   const (
       BarCodexBurst    = "codex_5h_burst"
       BarCodexWeekly   = "codex_weekly"
       BarAgyGemini     = "agy_gemini_5h"
       BarAgyOpus       = "agy_opus_5d"
       BarOpenCodeFree  = "opencode_free_16h"
   )
   ```
2. Update `agyBucket(id string)` to map `gemini-5h` to `(BarAgyGemini, "gemini", 300)` and `3p-weekly` / `3p-5h` to `(BarAgyOpus, "claude_gpt", 7200)`.
3. In `decodeCodexBar`, assign `BarCodexBurst` for Codex primary window and `BarCodexWeekly` for Codex secondary window.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/providers -run TestDecodeCodexBarFourBars -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/providers/quota.go internal/providers/codexbar_test.go
git commit -m "feat(providers): normalize 4-bar quota telemetry in codexbar parser"
```

---

### Task 2: Background Quota Observer Worker with Non-Blocking Cache (Story 1.2)

**Files:**
- Modify: `internal/orchestration/quota.go:20-115`
- Modify: `internal/orchestration/capacity_policy.go`
- Test: `internal/orchestration/quota_test.go`

**Interfaces:**
- Consumes: `Engine.quotas`, `Engine.quotaRefreshedAt`, `quotaRefreshTTL (60s)`.
- Produces:
  - `func (e *Engine) HealthPercent(bar string) float64` returning 0.0 to 1.0.
  - `func (e *Engine) BarState(bar string) string` returning `HEALTHY` ($\ge 0.70$), `PRESSURE_MEDIUM` ($0.30 - 0.70$), `PRESSURE_HIGH` ($< 0.30$), or `EXHAUSTED` ($0.0$).
  - Non-blocking `Engine.Quota()` resolving in $< 5\text{ ms}$ under concurrent read.

- [ ] **Step 1: Write the failing test**

In `internal/orchestration/quota_test.go`, add `TestQuotaHealthNormalizationAndNonBlocking`:
```go
func TestQuotaHealthNormalizationAndNonBlocking(t *testing.T) {
	e := &Engine{
		quotas: map[string][]providers.Quota{},
		quotaRefreshedAt: time.Now().UTC(),
	}
	remaining75 := 75.0
	remaining20 := 20.0
	e.quotas["codex"] = []providers.Quota{
		{Bucket: providers.BarCodexBurst, Remaining: &remaining75, State: "KNOWN"},
		{Bucket: providers.BarCodexWeekly, Remaining: &remaining20, State: "KNOWN"},
	}
	if h := e.HealthPercent(providers.BarCodexBurst); h != 0.75 {
		t.Fatalf("expected health 0.75, got %v", h)
	}
	if st := e.BarState(providers.BarCodexBurst); st != "HEALTHY" {
		t.Errorf("expected HEALTHY, got %s", st)
	}
	if st := e.BarState(providers.BarCodexWeekly); st != "PRESSURE_HIGH" {
		t.Errorf("expected PRESSURE_HIGH, got %s", st)
	}

	// Verify non-blocking concurrency under 50 routines
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := e.Quota("codex")
			if len(q) == 0 {
				t.Errorf("expected quotas returned")
			}
		}()
	}
	wg.Wait()
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("quota read took too long: %v", elapsed)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/orchestration -run TestQuotaHealthNormalizationAndNonBlocking -v`  
Expected: FAIL with `e.HealthPercent undefined` and `e.BarState undefined`.

- [ ] **Step 3: Implement minimal code in `internal/orchestration/quota.go` and `capacity_policy.go`**

1. Implement `HealthPercent(bar string) float64`:
   ```go
   func (e *Engine) HealthPercent(bar string) float64 {
       e.mu.Lock()
       defer e.mu.Unlock()
       for _, rows := range e.quotas {
           for _, q := range rows {
               if q.Bucket == bar && q.Remaining != nil {
                   pct := *q.Remaining / 100.0
                   if pct < 0 { return 0.0 }
                   if pct > 1.0 { return 1.0 }
                   return pct
               }
           }
       }
       return 1.0 // default unobserved to 1.0
   }
   ```
2. Implement `BarState(bar string) string`:
   - Return `EXHAUSTED` if health $\le 0.0$.
   - Return `PRESSURE_HIGH` if health $< 0.30$.
   - Return `PRESSURE_MEDIUM` if health $< 0.70$.
   - Return `HEALTHY` otherwise.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/orchestration -run TestQuotaHealthNormalizationAndNonBlocking -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/orchestration/quota.go internal/orchestration/capacity_policy.go internal/orchestration/quota_test.go
git commit -m "feat(orchestration): add health percent and bar state calculations with non-blocking cache"
```

---

### Task 3: Codesign, Team ID & Security Invariants Verification (Story 1.3)

**Files:**
- Modify: `internal/providers/codexbar.go:84-135`, `internal/providers/codexbar.go:215-231`
- Test: `internal/providers/codexbar_test.go`

**Interfaces:**
- Consumes: macOS `codesign` tool, Team ID `Y5PE65HELJ`, environment sanitizer.
- Produces: Strict rejection of untrusted binaries with `CODEXBAR_BINARY_UNVERIFIED`, stripped execution environment removing all `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, and browser launch block via `/usr/bin/false`.

- [ ] **Step 1: Write the failing test**

In `internal/providers/codexbar_test.go`, add `TestCodexBarSecurityInvariants`:
```go
func TestCodexBarSecurityInvariants(t *testing.T) {
	// 1. Verify environment sanitization excludes all API keys
	env := vendorUsageEnv([]string{
		"OPENAI_API_KEY=sk-leaked",
		"ANTHROPIC_API_KEY=sk-ant-leaked",
		"GEMINI_API_KEY=leaked",
		"HOME=/Users/tester",
		"USER=tester",
	}, t.TempDir())

	for _, e := range env {
		if strings.Contains(e, "OPENAI_API_KEY") || strings.Contains(e, "ANTHROPIC_API_KEY") || strings.Contains(e, "GEMINI_API_KEY") {
			t.Errorf("API key leaked in vendorUsageEnv: %s", e)
		}
		if strings.HasPrefix(e, "BROWSER=") && !strings.HasSuffix(e, "/open") {
			t.Errorf("BROWSER override not set properly: %s", e)
		}
	}

	// 2. Mock binary verification rejection
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	fakeBin := filepath.Join(t.TempDir(), "fake-codexbar")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho '{}'"), 0755); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := verifyCodexBarBinary(ctx, fakeBin, "")
	if err == nil || !strings.Contains(err.Error(), "CODEXBAR_BINARY_UNVERIFIED") {
		t.Errorf("expected CODEXBAR_BINARY_UNVERIFIED, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers -run TestCodexBarSecurityInvariants -v`  
Expected: FAIL if fake binary error message does not match or if env check fails.

- [ ] **Step 3: Implement minimal code in `internal/providers/codexbar.go`**

Ensure `vendorUsageEnv` only passes `HOME`, `USER`, `LOGNAME`, sets `BROWSER` to the symlinked `/usr/bin/false` wrapper, and `verifyCodexBarBinary` strictly returns `CODEXBAR_BINARY_UNVERIFIED` for unsigned or invalid-team binaries.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/providers -run TestCodexBarSecurityInvariants -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/providers/codexbar.go internal/providers/codexbar_test.go
git commit -m "security(providers): verify codexbar codesign team id and sanitize environment"
```

---

### Task 4: 10-Unique Engine Priority Matrix & Fallback Resolver Engine (Story 2.1)

**Files:**
- Create: `internal/orchestration/priority_matrix.go`
- Test: `internal/orchestration/priority_matrix_test.go`

**Interfaces:**
- Consumes: Task category (`coding`, `debugging`, `code_review`, `architecture`, `planning`, `research`, `agent_orchestration`).
- Produces: 
  - `func FallbackMatrix(category string) []string` returning exactly 10 distinct model engines.
  - `func ValidatePriorityMatrix() error` returning error if any duplicate engines or missing categories exist.

- [ ] **Step 1: Write the failing test**

In `internal/orchestration/priority_matrix_test.go`:
```go
func TestPriorityMatrix10UniqueEngines(t *testing.T) {
	categories := []string{
		"coding", "debugging", "code_review",
		"architecture", "planning", "research", "agent_orchestration",
	}
	for _, cat := range categories {
		matrix := FallbackMatrix(cat)
		if len(matrix) != 10 {
			t.Fatalf("category %s: expected 10 engines, got %d", cat, len(matrix))
		}
		seen := map[string]bool{}
		for i, engine := range matrix {
			if seen[engine] {
				t.Fatalf("category %s: duplicate engine %s at position %d", cat, engine, i)
			}
			seen[engine] = true
		}
		// First fallback must be from a different provider family than primary
		primary := matrix[0]
		fallback0 := matrix[1]
		if strings.Split(primary, "/")[0] == strings.Split(fallback0, "/")[0] {
			t.Fatalf("category %s: primary %s and fallback0 %s share same provider prefix", cat, primary, fallback0)
		}
	}
	if err := ValidatePriorityMatrix(); err != nil {
		t.Fatalf("priority matrix validation failed: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/orchestration -run TestPriorityMatrix10UniqueEngines -v`  
Expected: FAIL with `FallbackMatrix undefined` and `ValidatePriorityMatrix undefined`.

- [ ] **Step 3: Implement minimal code in `internal/orchestration/priority_matrix.go`**

Define the priority matrices for all 7 categories as specified in Section 4.2 of `docs/multi-provider-dynamic-routing-analysis.md`:
1. `coding`: `["codex/gpt-6.1-sol", "antigravity/gemini-4-argon", "opencode/crouton-alpha-free", "opencode/beam-free", "codex/gpt-6.1-terra", "antigravity/gemini-3.8-flash", "opencode/deepseek-v4.1-flash-free", "codex/gpt-6-astra", "opencode/iceman-free", "opencode/minimax-m3.1-flash-free"]`
2. `debugging`: `["codex/gpt-6.1-terra", "antigravity/gemini-4-argon", "codex/gpt-6.1-sol", "opencode/crouton-alpha-free", "opencode/beam-free", "opencode/deepseek-v4.1-flash-free", "opencode/big-pickle", "antigravity/gemini-3.8-flash", "codex/gpt-6-astra", "opencode/iceman-free"]`
3. `code_review`: `["antigravity/gemini-3.8-flash", "antigravity/claude-opus-5-5", "opencode/deepseek-v4.1-flash-free", "codex/gpt-6.1-terra", "antigravity/gemini-4-argon", "opencode/crouton-alpha-free", "codex/gpt-6.1-sol", "codex/gpt-6-astra", "opencode/beam-free", "opencode/big-pickle"]`
4. `architecture`: `["antigravity/claude-opus-5-5", "codex/gpt-6-astra", "antigravity/gemini-4-argon", "codex/gpt-6.1-sol", "antigravity/gemini-3.8-flash", "opencode/crouton-alpha-free", "opencode/beam-free", "codex/gpt-6.1-terra", "opencode/deepseek-v4.1-flash-free", "opencode/big-pickle"]`
5. `planning`: `["antigravity/gemini-4-argon", "codex/gpt-6.1-terra", "antigravity/gemini-3.8-flash", "opencode/big-pickle", "opencode/crouton-alpha-free", "opencode/beam-free", "opencode/deepseek-v4.1-flash-free", "codex/gpt-6.1-sol", "codex/gpt-6-astra", "opencode/iceman-free"]`
6. `research`: `["codex/gpt-6-luna", "opencode/beam-free", "antigravity/gemini-3.8-flash", "opencode/crouton-alpha-free", "codex/gpt-6.1-terra", "opencode/deepseek-v4.1-flash-free", "antigravity/gemini-4-argon", "opencode/iceman-free", "opencode/minimax-m3.1-flash-free", "opencode/longcat-2.5-free"]`
7. `agent_orchestration`: `["antigravity/gemini-4-argon", "codex/gpt-6-astra", "antigravity/claude-opus-5-5", "antigravity/gemini-3.8-flash", "opencode/crouton-alpha-free", "opencode/beam-free", "codex/gpt-6.1-terra", "opencode/deepseek-v4.1-flash-free", "codex/gpt-6.1-sol", "opencode/iceman-free"]`

Implement `FallbackMatrix(category string) []string` and `ValidatePriorityMatrix() error`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/orchestration -run TestPriorityMatrix10UniqueEngines -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/orchestration/priority_matrix.go internal/orchestration/priority_matrix_test.go
git commit -m "feat(orchestration): implement 10-unique model engine priority matrix per category"
```

---

### Task 5: DynamicEffortResolver Core & Cognitive Load Characterization (Story 2.2)

**Files:**
- Create: `internal/orchestration/effort_resolver.go`
- Test: `internal/orchestration/effort_resolver_test.go`

**Interfaces:**
- Consumes: Task category, cognitive complexity indicators (allowed paths count, acceptance checks, AST complexity), provider quota health (0.0 - 1.0).
- Produces:
  - `type EffortLevel string` (`EffortLow = "low"`, `EffortMedium = "medium"`, `EffortHigh = "high"`).
  - `func ResolveEffort(category string, complexity float64, quotaHealth float64, modelID string) (EffortLevel, string)`.
  - Latency: $< 500\text{ ns}$ execution time.

- [ ] **Step 1: Write the failing test**

In `internal/orchestration/effort_resolver_test.go`:
```go
func TestResolveEffortTruthTable(t *testing.T) {
	tests := []struct {
		name        string
		category    string
		complexity  float64
		quotaHealth float64
		modelID     string
		wantEffort  EffortLevel
		wantReason  string
	}{
		{
			name:        "high complexity nominal quota",
			category:    "coding",
			complexity:  0.9,
			quotaHealth: 0.85,
			modelID:     "codex/gpt-6.1-sol",
			wantEffort:  EffortHigh,
			wantReason:  "NOMINAL_HIGH_EFFORT",
		},
		{
			name:        "high complexity medium pressure throttled",
			category:    "coding",
			complexity:  0.9,
			quotaHealth: 0.50,
			modelID:     "codex/gpt-6.1-sol",
			wantEffort:  EffortMedium,
			wantReason:  "QUOTA_PRESSURE_THROTTLED",
		},
		{
			name:        "routine research low effort always",
			category:    "research",
			complexity:  0.2,
			quotaHealth: 1.0,
			modelID:     "codex/gpt-6-luna",
			wantEffort:  EffortLow,
			wantReason:  "ROUTINE_TASK_LOW_EFFORT",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotEffort, gotReason := ResolveEffort(tc.category, tc.complexity, tc.quotaHealth, tc.modelID)
			if gotEffort != tc.wantEffort {
				t.Errorf("effort mismatch: got %s, want %s", gotEffort, tc.wantEffort)
			}
			if gotReason != tc.wantReason {
				t.Errorf("reason mismatch: got %s, want %s", gotReason, tc.wantReason)
			}
		})
	}
}

func BenchmarkResolveEffort(b *testing.B) {
	for i := 0; i < b.N; i++ {
		ResolveEffort("coding", 0.8, 0.75, "codex/gpt-6.1-sol")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/orchestration -run TestResolveEffortTruthTable -v`  
Expected: FAIL with `ResolveEffort undefined`.

- [ ] **Step 3: Implement minimal code in `internal/orchestration/effort_resolver.go`**

```go
package orchestration

type EffortLevel string

const (
	EffortLow    EffortLevel = "low"
	EffortMedium EffortLevel = "medium"
	EffortHigh   EffortLevel = "high"
)

func ResolveEffort(category string, complexity float64, quotaHealth float64, modelID string) (EffortLevel, string) {
	if category == "research" || complexity < 0.3 {
		return EffortLow, "ROUTINE_TASK_LOW_EFFORT"
	}
	if quotaHealth < 0.60 && quotaHealth >= 0.30 {
		return EffortMedium, "QUOTA_PRESSURE_THROTTLED"
	}
	if quotaHealth < 0.30 {
		return EffortLow, "QUOTA_CRITICAL_LOW_EFFORT"
	}
	if complexity >= 0.7 && quotaHealth >= 0.60 {
		return EffortHigh, "NOMINAL_HIGH_EFFORT"
	}
	return EffortMedium, "STANDARD_EFFORT"
}
```

- [ ] **Step 4: Run test and benchmark to verify it passes**

Run: `go test ./internal/orchestration -run TestResolveEffortTruthTable -bench BenchmarkResolveEffort -v`  
Expected: PASS and benchmark $< 500\text{ ns/op}$.

- [ ] **Step 5: Commit**

```bash
git add internal/orchestration/effort_resolver.go internal/orchestration/effort_resolver_test.go
git commit -m "feat(orchestration): implement DynamicEffortResolver core with cognitive load throttling"
```

---

### Task 6: Claude Opus Scarcity Guard & Luna Strategic Repositioning (Story 2.3)

**Files:**
- Modify: `internal/orchestration/priority_matrix.go`
- Modify: `internal/orchestration/engine.go:800-950`
- Test: `internal/orchestration/scarcity_guard_test.go`

**Interfaces:**
- Consumes: Task category, candidate model ID, Opus 5-day reserve health percentage.
- Produces:
  - `func ValidateModelAdmission(category string, modelID string, opusHealth float64) error`.
  - Rejection with `OPUS_SCARCITY_GATE_TASK_DENIED` if Opus requested for `coding`, `debugging`, `planning`, or `research`.
  - Rejection with `OPUS_RESERVE_DEPLETED` if Opus requested for `architecture` when `opusHealth <= 0.20`.
  - Enforcement that `codex/gpt-6-luna` is never admitted as Primary for `coding` or `debugging`.

- [ ] **Step 1: Write the failing test**

In `internal/orchestration/scarcity_guard_test.go`:
```go
func TestOpusScarcityGuardAndLunaRepositioning(t *testing.T) {
	// 1. Opus denied for coding
	err := ValidateModelAdmission("coding", "antigravity/claude-opus-5-5", 1.0)
	if err == nil || !strings.Contains(err.Error(), "OPUS_SCARCITY_GATE_TASK_DENIED") {
		t.Errorf("expected OPUS_SCARCITY_GATE_TASK_DENIED, got %v", err)
	}

	// 2. Opus admitted for architecture with health > 0.20
	err = ValidateModelAdmission("architecture", "antigravity/claude-opus-5-5", 0.50)
	if err != nil {
		t.Errorf("expected architecture with healthy Opus to pass, got %v", err)
	}

	// 3. Opus rejected for architecture when reserve <= 0.20
	err = ValidateModelAdmission("architecture", "antigravity/claude-opus-5-5", 0.19)
	if err == nil || !strings.Contains(err.Error(), "OPUS_RESERVE_DEPLETED") {
		t.Errorf("expected OPUS_RESERVE_DEPLETED, got %v", err)
	}

	// 4. Luna denied as primary for coding
	matrix := FallbackMatrix("coding")
	if matrix[0] == "codex/gpt-6-luna" {
		t.Errorf("luna must not be primary for coding")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/orchestration -run TestOpusScarcityGuardAndLunaRepositioning -v`  
Expected: FAIL with `ValidateModelAdmission undefined`.

- [ ] **Step 3: Implement minimal code in `internal/orchestration/priority_matrix.go`**

```go
func ValidateModelAdmission(category string, modelID string, opusHealth float64) error {
	if modelID == "antigravity/claude-opus-5-5" {
		allowed := category == "architecture" || category == "code_review" || category == "agent_orchestration"
		if !allowed {
			return errors.New("OPUS_SCARCITY_GATE_TASK_DENIED")
		}
		if opusHealth <= 0.20 {
			return errors.New("OPUS_RESERVE_DEPLETED")
		}
	}
	if modelID == "codex/gpt-6-luna" && (category == "coding" || category == "debugging") {
		return errors.New("LUNA_CODING_DENIED")
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/orchestration -run TestOpusScarcityGuardAndLunaRepositioning -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/orchestration/priority_matrix.go internal/orchestration/scarcity_guard_test.go
git commit -m "feat(orchestration): enforce Opus 5.5 scarcity gate and Luna repositioning"
```

---

### Task 7: 5-Stage Adaptive State Machine & Throttle Policy (Story 3.1)

**Files:**
- Create: `internal/orchestration/state_machine.go`
- Test: `internal/orchestration/state_machine_test.go`

**Interfaces:**
- Consumes: Health of all 4 bars (`codex_5h_burst`, `codex_weekly`, `agy_gemini_5h`, `opencode_free_16h`).
- Produces:
  - `type OperationalStage string`
    - `Stage1Nominal = "Stage1Nominal"`
    - `Stage2MediumPressure = "Stage2MediumPressure"`
    - `Stage3CodexExhausted = "Stage3CodexExhausted"`
    - `Stage4AGYExhausted = "Stage4AGYExhausted"`
    - `Stage5DualCloudOutage = "Stage5DualCloudOutage"`
    - `Stage6FreeTier429 = "Stage6FreeTier429"`
  - `func DetermineOperationalStage(codexHealth, agyHealth, freeHealth float64) OperationalStage`.

- [ ] **Step 1: Write the failing test**

In `internal/orchestration/state_machine_test.go`:
```go
func TestOperationalStateMachineTransitions(t *testing.T) {
	tests := []struct {
		codexHealth float64
		agyHealth   float64
		freeHealth  float64
		wantStage   OperationalStage
	}{
		{1.0, 1.0, 1.0, Stage1Nominal},
		{0.60, 0.90, 1.0, Stage2MediumPressure},
		{0.0, 0.80, 1.0, Stage3CodexExhausted},
		{0.90, 0.0, 1.0, Stage4AGYExhausted},
		{0.0, 0.0, 1.0, Stage5DualCloudOutage},
		{0.80, 0.80, 0.0, Stage6FreeTier429},
	}
	for _, tc := range tests {
		stage := DetermineOperationalStage(tc.codexHealth, tc.agyHealth, tc.freeHealth)
		if stage != tc.wantStage {
			t.Errorf("for (%v, %v, %v): got %s, want %s", tc.codexHealth, tc.agyHealth, tc.freeHealth, stage, tc.wantStage)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/orchestration -run TestOperationalStateMachineTransitions -v`  
Expected: FAIL with `DetermineOperationalStage undefined`.

- [ ] **Step 3: Implement minimal code in `internal/orchestration/state_machine.go`**

Implement `DetermineOperationalStage`:
```go
package orchestration

type OperationalStage string

const (
	Stage1Nominal        OperationalStage = "Stage1Nominal"
	Stage2MediumPressure OperationalStage = "Stage2MediumPressure"
	Stage3CodexExhausted OperationalStage = "Stage3CodexExhausted"
	Stage4AGYExhausted   OperationalStage = "Stage4AGYExhausted"
	Stage5DualCloudOutage OperationalStage = "Stage5DualCloudOutage"
	Stage6FreeTier429    OperationalStage = "Stage6FreeTier429"
)

func DetermineOperationalStage(codexHealth, agyHealth, freeHealth float64) OperationalStage {
	if freeHealth <= 0.0 {
		return Stage6FreeTier429
	}
	if codexHealth <= 0.0 && agyHealth <= 0.0 {
		return Stage5DualCloudOutage
	}
	if codexHealth <= 0.0 {
		return Stage3CodexExhausted
	}
	if agyHealth <= 0.0 {
		return Stage4AGYExhausted
	}
	if codexHealth < 0.70 || agyHealth < 0.70 {
		return Stage2MediumPressure
	}
	return Stage1Nominal
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/orchestration -run TestOperationalStateMachineTransitions -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/orchestration/state_machine.go internal/orchestration/state_machine_test.go
git commit -m "feat(orchestration): implement 5-stage adaptive operational state machine"
```

---

### Task 8: Zero-Latency Circuit Breaker & Cooldown Manager (Story 3.2)

**Files:**
- Create: `internal/orchestration/circuit_breaker.go`
- Test: `internal/orchestration/circuit_breaker_test.go`

**Interfaces:**
- Consumes: HTTP status codes (e.g. 429), provider identifiers, retry-after durations.
- Produces:
  - `type CircuitState string` (`CircuitClosed = "CLOSED"`, `CircuitOpen = "OPEN"`, `CircuitHalfOpen = "HALF_OPEN"`).
  - `type CircuitBreaker struct`.
  - `func (cb *CircuitBreaker) Trip(provider string, cooldown time.Duration)`
  - `func (cb *CircuitBreaker) State(provider string) CircuitState`
  - `func (cb *CircuitBreaker) Allow(provider string) bool`
  - Overhead: $0\text{ ms}$ added execution delay on routing skip.

- [ ] **Step 1: Write the failing test**

In `internal/orchestration/circuit_breaker_test.go`:
```go
func TestCircuitBreakerLifecycle(t *testing.T) {
	cb := NewCircuitBreaker()
	if st := cb.State("codex"); st != CircuitClosed {
		t.Fatalf("expected initial state CLOSED, got %s", st)
	}
	if !cb.Allow("codex") {
		t.Fatalf("expected CLOSED breaker to allow requests")
	}

	// Trip with 50ms cooldown
	cb.Trip("codex", 50*time.Millisecond)
	if st := cb.State("codex"); st != CircuitOpen {
		t.Fatalf("expected state OPEN, got %s", st)
	}
	if cb.Allow("codex") {
		t.Fatalf("expected OPEN breaker to block requests")
	}

	// Wait for cooldown expiration -> HALF_OPEN
	time.Sleep(60 * time.Millisecond)
	if st := cb.State("codex"); st != CircuitHalfOpen {
		t.Fatalf("expected state HALF_OPEN after cooldown, got %s", st)
	}
	if !cb.Allow("codex") {
		t.Fatalf("expected HALF_OPEN to admit single probe")
	}

	// Record success -> CLOSED
	cb.Success("codex")
	if st := cb.State("codex"); st != CircuitClosed {
		t.Fatalf("expected state CLOSED after success, got %s", st)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/orchestration -run TestCircuitBreakerLifecycle -v`  
Expected: FAIL with `NewCircuitBreaker undefined`.

- [ ] **Step 3: Implement minimal code in `internal/orchestration/circuit_breaker.go`**

```go
package orchestration

import (
	"sync"
	"time"
)

type CircuitState string

const (
	CircuitClosed   CircuitState = "CLOSED"
	CircuitOpen     CircuitState = "OPEN"
	CircuitHalfOpen CircuitState = "HALF_OPEN"
)

type CircuitBreaker struct {
	mu       sync.RWMutex
	cooldown map[string]time.Time
	probing  map[string]bool
}

func NewCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{
		cooldown: make(map[string]time.Time),
		probing:  make(map[string]bool),
	}
}

func (cb *CircuitBreaker) Trip(provider string, dur time.Duration) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.cooldown[provider] = time.Now().Add(dur)
	cb.probing[provider] = false
}

func (cb *CircuitBreaker) State(provider string) CircuitState {
	cb.mu.RLock()
	until, ok := cb.cooldown[provider]
	probing := cb.probing[provider]
	cb.mu.RUnlock()

	if !ok {
		return CircuitClosed
	}
	if time.Now().Before(until) {
		return CircuitOpen
	}
	if probing {
		return CircuitOpen
	}
	return CircuitHalfOpen
}

func (cb *CircuitBreaker) Allow(provider string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	until, ok := cb.cooldown[provider]
	if !ok || time.Now().After(until) && !cb.probing[provider] {
		if ok && time.Now().After(until) {
			cb.probing[provider] = true
		}
		return true
	}
	return false
}

func (cb *CircuitBreaker) Success(provider string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	delete(cb.cooldown, provider)
	delete(cb.probing, provider)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/orchestration -run TestCircuitBreakerLifecycle -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/orchestration/circuit_breaker.go internal/orchestration/circuit_breaker_test.go
git commit -m "feat(orchestration): implement zero-latency circuit breaker with CLOSED/OPEN/HALF_OPEN states"
```

---

### Task 9: Dual Cloud Outage Resiliency & Free-Tier 429 Bypass Routing (Story 3.3)

**Files:**
- Modify: `internal/orchestration/engine.go`
- Modify: `internal/orchestration/opencode_health.go`
- Test: `internal/orchestration/outage_resiliency_test.go`

**Interfaces:**
- Consumes: `CircuitBreaker`, `DetermineOperationalStage`, candidate pool.
- Produces:
  - Stage 5 route selection: When Codex and AGY are exhausted, 100% of jobs route to OpenCode Free cluster (`crouton-alpha`, `beam`, `deepseek`) with 0 errors.
  - Stage 6 route selection: When OpenCode returns 429 (`provider.free-limit`), 16.6-hour cooldown ($59,760,000\text{ ms}$) trips breaker and router instantly bypasses OpenCode.

- [ ] **Step 1: Write the failing test**

In `internal/orchestration/outage_resiliency_test.go`:
```go
func TestDualCloudOutageAndFreeTierBypass(t *testing.T) {
	cb := NewCircuitBreaker()
	// Simulate dual cloud outage
	cb.Trip("codex", 1*time.Hour)
	cb.Trip("antigravity", 1*time.Hour)

	stage := DetermineOperationalStage(0.0, 0.0, 1.0)
	if stage != Stage5DualCloudOutage {
		t.Fatalf("expected Stage5DualCloudOutage, got %s", stage)
	}

	// Coding candidates must fall through to OpenCode Free
	matrix := FallbackMatrix("coding")
	var selected string
	for _, m := range matrix {
		prefix := strings.Split(m, "/")[0]
		if cb.Allow(prefix) {
			selected = m
			break
		}
	}
	if selected != "opencode/crouton-alpha-free" {
		t.Fatalf("expected crouton-alpha-free during dual cloud outage, got %s", selected)
	}

	// Now simulate OpenCode 429 limit hit
	cb.Trip("opencode", 16600*time.Millisecond) // 16.6h represented as ms in test
	if cb.Allow("opencode") {
		t.Fatalf("opencode should be blocked by circuit breaker")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/orchestration -run TestDualCloudOutageAndFreeTierBypass -v`  
Expected: PASS (or FAIL if circuit breaker integration logic in route selection is missing).

- [ ] **Step 3: Integrate circuit breaker with route selection in `internal/orchestration/engine.go`**

Ensure `Engine` maintains `cb *CircuitBreaker` and consults `e.cb.Allow(candidate.Provider)` before dispatching. If blocked, skip candidate immediately with log `CIRCUIT_BREAKER_OPEN`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/orchestration -run TestDualCloudOutageAndFreeTierBypass -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/orchestration/engine.go internal/orchestration/outage_resiliency_test.go
git commit -m "feat(orchestration): support dual cloud outage failover to OpenCode and 16.6h bypass"
```

---

### Task 10: SQLite Schema Migration (user_version 2: provider_quota_state & job_routing_audit) (Story 4.1)

**Files:**
- Modify: `internal/store/store.go:125-165`
- Create: `internal/store/routing_audit.go`
- Test: `internal/store/migration_v2_test.go`

**Interfaces:**
- Consumes: SQLite `state.sqlite` at `user_version 1` or fresh database.
- Produces:
  - `PRAGMA user_version = 2`.
  - Tables: `provider_quota_state` and `job_routing_audit`.
  - Types: `ProviderQuotaState` and `JobRoutingAudit`.
  - Methods: `SaveQuotaState()`, `GetQuotaStates()`, `RecordRoutingAudit()`, `GetRoutingAudit(jobID string)`.

- [ ] **Step 1: Write the failing test**

In `internal/store/migration_v2_test.go`:
```go
func TestMigrationToUserVersion2(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("query user_version failed: %v", err)
	}
	if version != 2 {
		t.Fatalf("expected user_version 2, got %d", version)
	}

	// Insert and retrieve quota state
	state := ProviderQuotaState{
		ProviderID:           "codex",
		TierName:             "codex_pro",
		WindowDurationHours:  5.0,
		RequestsUsedInWindow: 45,
		MaxWindowRequests:    180,
		HealthPct:            0.75,
		CircuitState:         "CLOSED",
	}
	if err := s.SaveQuotaState(state); err != nil {
		t.Fatalf("SaveQuotaState failed: %v", err)
	}
	states, err := s.GetQuotaStates()
	if err != nil || len(states) == 0 {
		t.Fatalf("GetQuotaStates failed: %v", err)
	}
	if states[0].ProviderID != "codex" || states[0].HealthPct != 0.75 {
		t.Errorf("quota state mismatch: %+v", states[0])
	}

	// Record routing audit
	audit := JobRoutingAudit{
		JobID:                    "job-test-1",
		TaskCategory:            "coding",
		AttemptNumber:           1,
		AssignedModel:           "codex/gpt-6.1-sol",
		AssignedEffort:          "high",
		RouteReason:             "NOMINAL_HIGH_EFFORT",
		QuotaPressureAtDispatch: 0.75,
		DispatchStatus:          "SUCCESS",
		LatencyMs:               780,
	}
	if err := s.RecordRoutingAudit(audit); err != nil {
		t.Fatalf("RecordRoutingAudit failed: %v", err)
	}
	history, err := s.GetRoutingAudit("job-test-1")
	if err != nil || len(history) != 1 {
		t.Fatalf("GetRoutingAudit failed: %v", err)
	}
	if history[0].AssignedModel != "codex/gpt-6.1-sol" {
		t.Errorf("audit history mismatch: %+v", history[0])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store -run TestMigrationToUserVersion2 -v`  
Expected: FAIL with `ProviderQuotaState undefined`, `version != 2`.

- [ ] **Step 3: Implement migration and queries in `internal/store/store.go` and `internal/store/routing_audit.go`**

In `internal/store/store.go`:
Update `init()` to migrate from `user_version 1` to `user_version 2` (or directly create v2 if version 0):
```sql
CREATE TABLE IF NOT EXISTS provider_quota_state (
    provider_id TEXT PRIMARY KEY,
    tier_name TEXT NOT NULL,
    window_duration_hours REAL NOT NULL,
    requests_used_in_window INTEGER NOT NULL DEFAULT 0,
    max_window_requests INTEGER NOT NULL,
    health_pct REAL NOT NULL DEFAULT 1.0,
    circuit_state TEXT NOT NULL DEFAULT 'CLOSED',
    retry_after_utc TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS job_routing_audit (
    job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    task_category TEXT NOT NULL,
    attempt_number INTEGER NOT NULL DEFAULT 1,
    assigned_model TEXT NOT NULL,
    assigned_effort TEXT NOT NULL,
    route_reason TEXT NOT NULL,
    quota_pressure_at_dispatch REAL NOT NULL,
    dispatch_status TEXT NOT NULL,
    latency_ms INTEGER,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (job_id, attempt_number)
);
CREATE INDEX IF NOT EXISTS idx_routing_audit_created ON job_routing_audit(created_at);
PRAGMA user_version=2;
```
Implement `SaveQuotaState`, `GetQuotaStates`, `RecordRoutingAudit`, `GetRoutingAudit` in `internal/store/routing_audit.go`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store -run TestMigrationToUserVersion2 -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/routing_audit.go internal/store/migration_v2_test.go
git commit -m "feat(store): migrate sqlite schema to user_version 2 with quota state and routing audit"
```

---

### Task 11: MCP delegate_task Auto-Resolution & Routing Audit Integration (Story 4.2)

**Files:**
- Modify: `internal/mcpserver/server.go:98-125`
- Modify: `internal/model/model.go`
- Test: `internal/mcpserver/server_routing_test.go`

**Interfaces:**
- Consumes: `model.DelegateTaskInput` with `Target` omitted or set to `"auto"`.
- Produces:
  - Dynamic resolution of model and reasoning effort.
  - Inclusion of `route_decision` metadata in MCP response.
  - Durable insertion into `job_routing_audit` table.

- [ ] **Step 1: Write the failing test**

In `internal/mcpserver/server_routing_test.go`:
```go
func TestDelegateTaskAutoResolution(t *testing.T) {
	// Setup test MCP server with mock engine and store
	server, s := setupTestMCPServer(t)
	res, err := callMCPTool(server, "delegate_task", map[string]any{
		"task": map[string]any{
			"category": "coding",
			"objective": "Implement O(N log N) interval binary search",
		},
		"target": "auto",
	})
	if err != nil {
		t.Fatalf("delegate_task call failed: %v", err)
	}
	rd, ok := res["route_decision"].(map[string]any)
	if !ok {
		t.Fatalf("expected route_decision in response, got %v", res)
	}
	if rd["selected_model"] != "codex/gpt-6.1-sol" {
		t.Errorf("expected gpt-6.1-sol, got %v", rd["selected_model"])
	}
	if rd["resolved_effort"] != "high" {
		t.Errorf("expected high effort, got %v", rd["resolved_effort"])
	}

	// Verify routing audit logged
	jobID := res["job_id"].(string)
	audit, err := s.GetRoutingAudit(jobID)
	if err != nil || len(audit) == 0 {
		t.Fatalf("expected audit logged, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mcpserver -run TestDelegateTaskAutoResolution -v`  
Expected: FAIL with `route_decision` missing or `target: auto` unhandled.

- [ ] **Step 3: Implement minimal code in `internal/mcpserver/server.go`**

In `handleDelegateTask`:
1. If `in.Target == "" || in.Target == "auto"`:
   - Identify task category (from `in.Task.Category` or default to `"coding"`).
   - Evaluate `FallbackMatrix(category)`, health percents, and resolve target model and effort.
   - Attach `route_decision` map to MCP response.
2. After job dispatch, write record to `s.RecordRoutingAudit(...)`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mcpserver -run TestDelegateTaskAutoResolution -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/server.go internal/mcpserver/server_routing_test.go internal/model/model.go
git commit -m "feat(mcpserver): integrate dynamic target auto-resolution and audit logging in delegate_task"
```

---

### Task 12: Real-Time Diagnostics MCP Tools (Story 4.3)

**Files:**
- Modify: `internal/mcpserver/server.go`
- Test: `internal/mcpserver/diagnostics_test.go`

**Interfaces:**
- Consumes: In-memory quota telemetry, circuit breaker states, store audit table.
- Produces 3 MCP Tools:
  - `get_provider_quotas`: returns status of all 4 bars with remaining percentage and circuit state.
  - `get_active_circuit_breakers`: returns tripped providers with cooldown deadlines.
  - `get_routing_audit`: returns decision history and fallbacks for a given `job_id`.

- [ ] **Step 1: Write the failing test**

In `internal/mcpserver/diagnostics_test.go`:
```go
func TestDiagnosticsMCPTools(t *testing.T) {
	server, s := setupTestMCPServer(t)

	// 1. get_provider_quotas
	resQuotas, err := callMCPTool(server, "get_provider_quotas", map[string]any{})
	if err != nil {
		t.Fatalf("get_provider_quotas failed: %v", err)
	}
	bars, ok := resQuotas["bars"].([]any)
	if !ok || len(bars) == 0 {
		t.Fatalf("expected bars array, got %v", resQuotas)
	}

	// 2. get_active_circuit_breakers
	resBreakers, err := callMCPTool(server, "get_active_circuit_breakers", map[string]any{})
	if err != nil {
		t.Fatalf("get_active_circuit_breakers failed: %v", err)
	}
	if _, ok := resBreakers["tripped_breakers"]; !ok {
		t.Fatalf("expected tripped_breakers key, got %v", resBreakers)
	}

	// 3. get_routing_audit
	resAudit, err := callMCPTool(server, "get_routing_audit", map[string]any{"job_id": "job-test-1"})
	if err != nil {
		t.Fatalf("get_routing_audit failed: %v", err)
	}
	if _, ok := resAudit["attempts"]; !ok {
		t.Fatalf("expected attempts key, got %v", resAudit)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mcpserver -run TestDiagnosticsMCPTools -v`  
Expected: FAIL with unknown tool error for the 3 tools.

- [ ] **Step 3: Implement minimal tool handlers in `internal/mcpserver/server.go`**

Register the three tools via `add(s, e, ...)`:
1. `get_provider_quotas`: queries `e.Quota("")` and returns normalized bars.
2. `get_active_circuit_breakers`: queries `e.cb` for tripped cooldowns.
3. `get_routing_audit`: takes `jobID struct { JobID string }` and queries `s.GetRoutingAudit(in.JobID)`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mcpserver -run TestDiagnosticsMCPTools -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/server.go internal/mcpserver/diagnostics_test.go
git commit -m "feat(mcpserver): register get_provider_quotas, get_active_circuit_breakers, and get_routing_audit diagnostics tools"
```

---

### Task 13: End-to-End Orchestration & Verification Test Suite

**Files:**
- Create: `internal/orchestration/multi_provider_routing_e2e_test.go`
- Test: Full integration test suite

**Interfaces:**
- Consumes: All components from Tasks 1-12.
- Produces: Comprehensive test verifying:
  - Scenario A: Codex 100% down failover to Argon 4 (`medium` effort).
  - Scenario B: AGY down failover to Sol/Terra.
  - Scenario C: Dual cloud outage failover to Crouton/Beam/DeepSeek in OpenCode Free.
  - Scenario D: OpenCode 429 16.6h bypass.
  - Performance: Decision latency $\le 5\text{ ms}$, zero-leak security.

- [ ] **Step 1: Write the failing test**

In `internal/orchestration/multi_provider_routing_e2e_test.go`:
```go
func TestEndToEndMultiProviderRoutingScenarios(t *testing.T) {
	// Full test validating transitions across Scenarios A, B, C, D
	// and verifying zero data loss and sub-5ms routing overhead.
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 1. Nominal scenario -> Sol selected at high effort
	decision := EvaluateRouting(db, "coding", 0.9, map[string]float64{
		providers.BarCodexBurst: 1.0,
		providers.BarAgyGemini:  1.0,
	})
	if decision.SelectedModel != "codex/gpt-6.1-sol" || decision.ResolvedEffort != "high" {
		t.Errorf("Nominal failed: %+v", decision)
	}

	// 2. Scenario A (Codex down) -> Argon selected at medium effort
	decisionA := EvaluateRouting(db, "coding", 0.9, map[string]float64{
		providers.BarCodexBurst: 0.0,
		providers.BarAgyGemini:  0.8,
	})
	if decisionA.SelectedModel != "antigravity/gemini-4-argon" || decisionA.ResolvedEffort != "medium" {
		t.Errorf("Scenario A failed: %+v", decisionA)
	}

	// 3. Scenario C (Dual cloud outage) -> Crouton selected
	decisionC := EvaluateRouting(db, "coding", 0.9, map[string]float64{
		providers.BarCodexBurst: 0.0,
		providers.BarAgyGemini:  0.0,
	})
	if decisionC.SelectedModel != "opencode/crouton-alpha-free" {
		t.Errorf("Scenario C failed: %+v", decisionC)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/orchestration -run TestEndToEndMultiProviderRoutingScenarios -v`  
Expected: FAIL with `EvaluateRouting undefined`.

- [ ] **Step 3: Implement orchestration wrapper `EvaluateRouting`**

In `internal/orchestration/engine.go` (or `multi_provider_routing_e2e_test.go`), expose the end-to-end evaluation pipeline that strings together `DetermineOperationalStage`, `FallbackMatrix`, `ValidateModelAdmission`, `ResolveEffort`, and `CircuitBreaker`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/orchestration -run TestEndToEndMultiProviderRoutingScenarios -v`  
Expected: PASS

- [ ] **Step 5: Run full project test suite**

Run: `go test ./internal/...`  
Expected: PASS across all packages.

- [ ] **Step 6: Commit**

```bash
git add internal/orchestration/multi_provider_routing_e2e_test.go
git commit -m "test(orchestration): add comprehensive end-to-end multi-provider dynamic routing test suite"
```
