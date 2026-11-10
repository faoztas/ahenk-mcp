# Model compatibility audit — 2026-11-10

The audit used the signed Antigravity CLI 1.5.3 and the pinned Intel OpenCode App 2.2.6. Discovery used `agy models`, `opencode models --standalone` in a fresh anonymous environment, and the current OpenCode machine catalog. The audit window began at `2026-11-10T13:53:06.15345Z`. [Machine-readable inventory](model-inventory-2026-11-10.json)

The `go-integer-v1` fixture asked for the final integer after two Go statements. A pass required the exact provider-reported identity, terminal success and the expected structured integer. The inventory records a stable evidence digest for each dispatched probe, usage when exposed, and the pinned binary fingerprint. The audit did not send checkout contents or create a workspace snapshot. Timing includes the adapter's preflight or session setup and supports no general quality ranking. Each successful result was reused in follow-on inventory passes.

## Antigravity

All 13 distinct selectable slugs passed. The subscription preflight used the existing native account context, a 15% reserve, `useG1Credits:false`, and the actual shared quota windows. The effective runtime credit setting remains `UNKNOWN`. Every passed row reported the exact requested model. Role evidence for native coding and independent review is reused from M2A for the pinned Gemini Flash medium slug. Other slugs are eligible only for synthetic read-only work when fresh evidence is explicitly loaded. [AGY Headless CLI](https://antigravity.google/docs/cli/headless/) · [M2A evidence](m2a-acceptance.md)

| Exact slug | Family and quota buckets | Effort | Inference | Latency | Coding | Review | Recommended role |
| --- | --- | --- | --- | ---: | --- | --- | --- |
| `claude-opus-5-5-high` | claude_gpt · `3p-5h`, `3p-weekly` | high | PASS | 14199 ms | NOT_TESTED | NOT_TESTED | Synthetic read-only |
| `claude-opus-5-5-low` | claude_gpt · `3p-5h`, `3p-weekly` | low | PASS | 19488 ms | NOT_TESTED | NOT_TESTED | Synthetic read-only |
| `claude-opus-5-5-medium` | claude_gpt · `3p-5h`, `3p-weekly` | medium | PASS | 13812 ms | NOT_TESTED | NOT_TESTED | Synthetic read-only |
| `claude-sonnet-5-5-high` | claude_gpt · `3p-5h`, `3p-weekly` | high | PASS | 14173 ms | NOT_TESTED | NOT_TESTED | Synthetic read-only |
| `claude-sonnet-5-5-low` | claude_gpt · `3p-5h`, `3p-weekly` | low | PASS | 14515 ms | NOT_TESTED | NOT_TESTED | Synthetic read-only |
| `claude-sonnet-5-5-medium` | claude_gpt · `3p-5h`, `3p-weekly` | medium | PASS | 15176 ms | NOT_TESTED | NOT_TESTED | Synthetic read-only |
| `gemini-3.8-flash-high` | gemini · `gemini-5h`, `gemini-weekly` | high | PASS | 33077 ms | NOT_TESTED | NOT_TESTED | Synthetic read-only |
| `gemini-3.8-flash-low` | gemini · `gemini-5h`, `gemini-weekly` | low | PASS | 15196 ms | NOT_TESTED | NOT_TESTED | Synthetic read-only |
| `gemini-3.8-flash-medium` | gemini · `gemini-5h`, `gemini-weekly` | medium | PASS | 19747 ms | PASS_M2A | PASS_M2A | Trusted-local coding and review |
| `gemini-4-argon-high` | gemini_argon · `gemini-argon-wtu-weekly` | high | PASS | 20093 ms | NOT_TESTED | NOT_TESTED | Synthetic read-only |
| `gemini-4-argon-low` | gemini_argon · `gemini-argon-wtu-weekly` | low | PASS | 18192 ms | NOT_TESTED | NOT_TESTED | Synthetic read-only |
| `gemini-4-argon-medium` | gemini_argon · `gemini-argon-wtu-weekly` | medium | PASS | 19554 ms | NOT_TESTED | NOT_TESTED | Synthetic read-only |
| `gpt-oss-120b-medium` | claude_gpt · `3p-5h`, `3p-weekly` | medium | PASS | 18392 ms | NOT_TESTED | NOT_TESTED | Synthetic read-only |

## OpenCode Free

Nine models passed with exact returned identity and zero reported cost. A historical or public price entry alone grants no access. Public-only zero input/output entries remain separate from an installed anonymous route. Null price fields in the JSON inventory denote undisclosed metadata. The active anonymous numerical quota and pool remain `UNKNOWN`. The audit made no repeat request for the upstream rejection.

| Exact ID | Interface | Cost evidence | Inference | Latency | Coding / review | Restriction or recommended role |
| --- | --- | --- | --- | ---: | --- | --- |
| `opencode/beam-free` | chat | `VERIFIED_FREE_INSTALLED` | PASS | 1982 ms | NOT_TESTED / NOT_TESTED | Synthetic read-only |
| `opencode/big-pickle` | chat | `VERIFIED_FREE_INSTALLED` | PASS | 2437 ms | NOT_TESTED / NOT_TESTED | Synthetic read-only |
| `opencode/crouton-alpha-free` | chat | `VERIFIED_FREE_INSTALLED` | UNSUPPORTED_INTERFACE | — | NOT_TESTED / NOT_TESTED | Pinned identity-verifying route unavailable |
| `opencode/deepseek-v4.1-flash-free` | chat | `VERIFIED_FREE_INSTALLED` | PASS | 3624 ms | NOT_TESTED / NOT_TESTED | Synthetic read-only |
| `opencode/flint-40b-free` | chat | `VERIFIED_FREE_INSTALLED` | PASS | 2430 ms | NOT_TESTED / NOT_TESTED | Synthetic read-only |
| `opencode/glm-5.3-flash-free` | chat | `VERIFIED_FREE_INSTALLED` | PASS | 1372 ms | PASS_M2A / PASS_M2A | Trusted-local coding and review |
| `opencode/iceman-free` | chat | `PUBLIC_ZERO_INPUT_OUTPUT` | MODEL_UNAVAILABLE | — | NOT_TESTED / NOT_TESTED | Public catalog only |
| `opencode/inkling-2-small-free` | chat | `PUBLIC_ZERO_INPUT_OUTPUT` | MODEL_UNAVAILABLE | — | NOT_TESTED / NOT_TESTED | Public catalog only |
| `opencode/jev-1.13-free` | decision | `PUBLIC_ZERO_INPUT_OUTPUT` | UNSUPPORTED_INTERFACE | — | NOT_SUPPORTED / NOT_SUPPORTED | Decision protocol requires a separate adapter and authorized access |
| `opencode/longcat-2.5-free` | chat | `VERIFIED_FREE_PUBLIC_PIN` | PASS | 3484 ms | NOT_TESTED / NOT_TESTED | Synthetic read-only |
| `opencode/mimo-v2.6-flash-free` | chat | `VERIFIED_FREE_INSTALLED` | PASS | 2420 ms | NOT_TESTED / NOT_TESTED | Synthetic read-only |
| `opencode/minimax-m3.1-flash-free` | chat | `PUBLIC_ZERO_INPUT_OUTPUT` | MODEL_UNAVAILABLE | — | NOT_TESTED / NOT_TESTED | Public catalog only |
| `opencode/nemotron-3.5-lightning-free` | chat | `VERIFIED_FREE_INSTALLED` | PASS | 2720 ms | NOT_TESTED / NOT_TESTED | Synthetic read-only |
| `opencode/nemotron-3.5-super-vl-free` | chat | `VERIFIED_FREE_INSTALLED` | PASS | 1516 ms | NOT_TESTED / NOT_TESTED | Synthetic read-only |
| `opencode/pulse-alpha-free` | chat | `VERIFIED_FREE_INSTALLED` | MODEL_UNAVAILABLE | 320 ms | NOT_TESTED / NOT_TESTED | Anonymous invocation rejected by upstream HTTP 400 |

The vendor documents geographic restrictions for Crouton Alpha, Inkling 2 Small and Beam. The audit did not infer the account's region from a failed route. The vendor page also mentions Nemotron 3 Ultra Free. It was absent from the active installed and machine-catalog candidates and received no invocation. [OpenCode Console model catalog and restrictions](https://opencode.ai/docs/console/models)

## Data handling

OpenCode publishes model-specific policies. Its catalog describes zero retention for GLM, DeepSeek and LongCat, 30-day safety retention for Beam, and possible model improvement use for several other Free models. Account-specific AGY retention and training settings were not inspected and remain `UNKNOWN`. The probes contained synthetic arithmetic only. [OpenCode Console privacy information](https://opencode.ai/docs/console/models) · [Antigravity data collection settings](https://antigravity.google/docs/settings)

## Refresh and routing

Run `ahenk audit-models --config FILE` for discovery and add `--live` for inference. During a single audit window, `--previous FILE` reuses recorded model results for the same pinned executable. The optional absolute `model_audit_file` configuration enables fresh verified candidates for up to 24 hours from the audit window's start. Current binary, cost, quota, policy and health gates still run at dispatch. Latency-first selection can break ties using measured audit latency. Existing M1 and M2A capability pins remain separate from text-inference observations.
