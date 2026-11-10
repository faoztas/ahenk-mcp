# OpenCode native transport evidence

## Pilot and controls

On 10 November 2026, OpenCode Desktop completed GLM and DeepSeek requests, while separate native sessions with all tools disabled returned upstream 429 responses. An isolated GLM pilot using `steps=2` and a single `read` tool configured to ask for permission returned the exact synthetic marker with a `stop` finish and zero reported cost. It produced no tool events. A local test double separately verified that denying a read request prevents file content from reaching the provider and that other tools stay disabled. [OpenCode P0 diagnostics](opencode-p0.md)

Ahenk added `native_read_ask` as an opt-in transport alongside the existing stateless route. The full Go suite, vet, race checks, and Intel macOS build passed at the time of the integration.

## Live acceptance

A single `delegate_task` from Codex App completed in one native attempt on `opencode/glm-5.3-flash-free`. OpenCode returned the exact synthetic marker, a `stop` finish, token usage, and zero reported cost. An independent check matched the result hash. Ahenk recorded `SCOPED_CONTEXT`, `PROCESS_EXIT`, `TOOL_FREE_TEXT`, and `ZERO_PRICE_CATALOG`. The worker released its lock, and the synthetic repository's HEAD, index, and marker file matched their preflight values.

The historical readiness report showed `OPENCODE_LIVE_READY=true` and `AGY_LIVE_READY=true`. Four stateless cooldowns stayed in place. The prior stateless GLM record had unknown upstream scope and appeared as `RECORDED_GATE_OTHER_TRANSPORT` for the native route. There was no active shared-scope gate. This completed G-005 and qualified the same route for the [M1 receiver](m1-build.md).
