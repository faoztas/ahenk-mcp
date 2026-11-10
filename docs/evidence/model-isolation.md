# OpenCode model limit isolation

## Routing behavior

OpenCode health and reservation keys include the account, upstream provider, model, and transport. Rate limits, access errors, transient failures, circuit state, and model reservations stay scoped to the affected route. An explicitly asserted `pool` or `account` limit also applies to its shared scope and appears under `opencode_shared_gates` in readiness output. The scheduler's two-job concurrency cap is separate from provider quota.

The quota view can expose `opencode_account`, `opencode_provider`, `health_scope`, `reason`, and `reset_at`. Readiness shows model state under `opencode_models`. Numeric capacity remains `UNKNOWN` without a measurement. A legacy local pool label does not establish shared upstream scope. Cooldown import preserves known account identity and expiration. A repeated import can fill missing fields while preserving the initial `ImportedAt` value.

## Verification

Offline tests covered independent 429 and access expiration across three exact models, SQLite restart, legacy record replay, upstream and account separation, route reservations and circuits, repeated imports, and M1 route selection. The baseline suite, vet, race checks, and Intel macOS build passed. Independent builds produced matching digests.

A historical live read-only check recorded four stateless model cooldowns and no shared-scope gate. A later successful native GLM attempt supplied live readiness evidence for its separate transport. [OpenCode diagnostics](opencode-p0.md) · [Native OpenCode acceptance](native-opencode.md)
