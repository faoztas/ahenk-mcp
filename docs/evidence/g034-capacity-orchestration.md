# G-034 adaptive quality and capacity

Alpha.3 keeps Codex as the controller. The scheduler filters by data policy, cost, demonstrated role and required independent review before selecting an eligible worker. Quality tiers are evidence categories, rather than numerical scores comparing providers. If the highest eligible tier is held by reserve, health, or quota, a lower tier cannot become a fallback. Comparable-quality work can use OpenCode Free to preserve AGY weekly capacity. Existing M2A coding and review evidence remains bound to the verified routes. The inference inventory grants scoped read-only eligibility and supplies no evidence for architecture, planning, general reasoning, or research roles.

## Capacity policy

The default AGY reserve is 15 percent for each weekly bucket. The scheduler accounts for a minimum two percentage-point dispatch buffer, recent same-bucket burn, and attempts since the last matched observation. The native AGY subscription preflight checks the effective weekly floor before a prompt. Each family has one in-flight reservation under the scheduler lock. For discretionary work, an observed trajectory that would cross the reserve before reset can defer dispatch. A new verified reset starts a fresh estimate for that bucket. Private pacing history retains at most 40 normalized observations and binds them to the configured AGY account context.

The reserve remains active if the controller weekly signal is unknown. A verified empty Codex weekly window releases the controller-dependent reserve. A repository-authorized override with a task justification bypasses the reserve for that task. Native cost and quota exhaustion checks still apply. Buffer and pacing values are estimates. External AGY usage, quota-reporting delays, and variable per-job consumption can change the balance.

Codex account-level warnings use the 20 percent low and 10 percent critical defaults independently per reported window. An absent five-hour account window is emitted as `UNKNOWN`. Extra model-specific windows stay separate. OpenCode Free capacity has no verified numeric balance. The scheduler keeps persisted model and transport cooldowns, and widens a cooldown only when the provider explicitly reports model, account or pool scope.

## Deterministic acceptance

| Scenarios | Verified result |
| --- | --- |
| T01–T03 | Stronger demonstrated tier wins. Equal tier can use OpenCode Free. A protected AGY reserve cannot lower the required tier. An authorized override is recorded. |
| T04–T07, T13, T17 | Low and exhausted windows produce distinct warnings. Only verified usable controller windows support the Codex-only warning. Missing and stale measurements stay unknown. |
| T08–T09, T15 | Gemini, Claude/GPT, and Argon keep distinct windows. Argon has no synthetic session window. Reset and pacing logic affect only the matching family. |
| T10–T11 | Model-level rate limits preserve other routes. Explicit pool scope gates the whole matching pool and expires at the recorded retry time. |
| T12, T16 | Canonical provider independence, role evidence, and verified cost admission remain mandatory. |
| T14, T18 | Concurrent dispatch can reserve a family once. A legacy SQLite job loads with its existing route and status semantics. |

## MCP warnings

| Code | Condition and action |
| --- | --- |
| `CODEX_ONLY_WARNING` | Verified controller windows are usable and the task has no eligible external route. Direct execution uses Codex quota. |
| `CODEX_CONTROLLER_LOW` | An observed controller window is at the low or critical threshold. Use concise batches with eligible workers. |
| `CODEX_5H_EXHAUSTED_OPENCODE_UNAVAILABLE` | The observed five-hour window is empty and OpenCode Free has no eligible route. The response includes AGY's availability or reserve state and the earliest known recovery. |
| `AGY_WEEKLY_RESERVE_PROTECTED` | The family is held by the protected floor or weekly pacing. Check the alternate route, reset, or task override policy. |
| `OPENCODE_FREE_UNAVAILABLE` | The task or a named model or pool is unavailable. Respect provider or scheduler cooldown provenance. |
| `ALL_ELIGIBLE_CAPACITY_BLOCKED` | The observed controller is exhausted and the task has no eligible external route. Inspect recovery times and rejected routes. |
| `CAPACITY_UNVERIFIED` | A fresh matched balance is missing. The response carries unknown capacity and identifies the evidence gap. |

The following snippets contain synthetic values. The actual responses also carry stable warning IDs, assessment times, observed timestamps when known, severity, and a recommended action.

```json
{
  "status": "BLOCKED",
  "reason_code": "AGY_WEEKLY_RESERVE_PROTECTED",
  "capacity_warnings": [
    {
      "code": "AGY_WEEKLY_RESERVE_PROTECTED",
      "provider": "agy",
      "bucket": "gemini-weekly",
      "scope": "gemini",
      "remaining_percent": 16,
      "threshold_percent": 15,
      "alternative": "NO_EQUIVALENT_OPENCODE_ROUTE",
      "consequence": "BLOCKED"
    }
  ]
}
```

```json
{
  "capacity_warnings": [
    {
      "code": "CAPACITY_UNVERIFIED",
      "provider": "codex",
      "bucket": "five_hour",
      "remaining_percent": null,
      "consequence": "CAPABILITY_CHECK"
    },
    {
      "code": "OPENCODE_FREE_UNAVAILABLE",
      "provider": "opencode",
      "scope": "model",
      "model": "opencode/glm-5.3-flash-free",
      "remaining_percent": null,
      "retry_at": "2026-11-11T00:00:00Z"
    }
  ]
}
```

## Live read-only acceptance

On 2026-11-10, a temporary Alpha.3 MCP owner used the pinned CodexBar executable and matched account metadata from the local Codex App server and signed AGY status line. The original configuration and state remained unchanged. The App account match is at the account level. The metadata does not distinguish workspaces sharing the same account.

| Capacity source | Live observation |
| --- | --- |
| Codex | Effective source `oauth`, account `MATCHED`, weekly `HEALTHY`, confidence `exact`. The account-level five-hour window was absent and emitted as `UNKNOWN`. |
| AGY | Requested source `cli`, effective source `app`, account `MATCHED`. Both Gemini and Claude/GPT windows and the Argon weekly window were `HEALTHY`. The source did not attest exact confidence. |
| OpenCode Free | Imported the persisted GLM model-level HTTP 429 gate. The reported retry boundary was approximately 2026-11-11 00:00 UTC. Numeric remaining capacity stayed `UNKNOWN`. |

The installed AGY binary had advanced from 1.5.3 to 1.5.4. The previous pin correctly closed the execution gate. The new signed 1.5.4 fingerprint was verified for quota reads only. A read-only utility preview selected the inference-verified `opencode/beam-free` route and excluded the GLM cooldown. An architecture submission reached a blocked decision with zero provider attempts. The MCP responses exposed structured warnings and concise human-readable text. The model audit and M2A coding evidence were reused. The live check made no inference calls and installed no provider updates. [AGY status-line protocol](https://antigravity.google/docs/cli/statusline)

## Final regression and package

The complete Go test suite, race suite, vet, and module verification passed. The regression includes SQLite compatibility, M1 handoff, M2A coding and review contracts, and T01–T18 capacity fixtures. The final package smoke verified all eight tools, three-source capacity fields, and `QUALITY_EVIDENCE_REQUIRED` with zero attempts for an architecture task. An initial regression found that a candidate with unavailable status and an empty reason could reach ranking. The final run passed after rejecting that candidate explicitly.

The CGO-disabled Intel macOS package reported `0.3.0-alpha.3`. Its x86-64 executable, byte-identical Apache-2.0 license, and dependency attributions were verified. Package SHA-256 is `8f9074c01e1795e57f3c515bb52e338705997b40d13a42e6dde8adf69f7a82d8`.
