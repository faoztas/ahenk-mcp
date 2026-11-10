# G-036 OpenCode Free availability reconciliation

## Root cause evidence

The retained Ahenk attempt from 2026-11-10 15:34:39 UTC used the pinned OpenCode App CLI 2.2.6, anonymous `opencode/glm-5.3-flash-free`, the isolated `native_read_ask` transport, and the official OpenCode inference route. The native session returned upstream HTTP 429, `retryAfterMs:30321000`, and no asserted scope. Ahenk added the duration once and obtained 2026-11-11 00:00:00.579 UTC. The arithmetic and UTC conversion match the stored record. The historical attempt came from synthetic child acceptance. It was retained outside the original session and imported into a separate capacity-check state.

The user confirmed the successful run was in the OpenCode desktop app. Its local database recorded a 2.2.6 assistant response at 16:53:25 UTC with canonical provider and model `opencode/glm-5.3-flash-free`, a normal stop, zero reported cost, and a succeeded idle state. The session used another project directory. The running desktop CLI used an alternate on-disk copy with the same SHA-256 as the pinned App binary, `bfd1f79c2fb005246c56d0e9fb472206c22782d065872d71c26f01f3eb3a0a82`. A separate CLI on PATH was 2.2.0 with a different fingerprint and was outside this comparison. The app's private local model and integration endpoints returned HTTP 401 without its credentials. The desktop session's historical authentication mode, endpoint, and pool therefore remain unverified. The account store, transcript text, and credentials were excluded from the comparison.

Two scheduler behaviors explain the stale availability report. It treated an unscoped native retry as a hard gate on later Ahenk sessions of the same model and transport. Its health reducer also ignored successful outcomes for cooldown reconciliation and retained the gate with the longest deadline. The desktop result proves the model answered in that desktop session. It does not establish that the desktop and Ahenk sessions shared the anonymous quota pool. OpenCode 2.2.6 separately caps its own per-retry wait at 15 minutes, which is a scheduling rule rather than evidence of a provider quota reset. [OpenCode 2.2.6 retry policy](https://github.com/anomalyco/opencode/blob/v2.2.6/packages/core/src/model-retry.ts)

## Reconciliation and live proof

Health is reduced from durable SQLite attempt observations and the existing import records. A verified success must match the pinned binary, canonical model, anonymous account, endpoint, and transport. Its start must follow the failure observation. Session-scoped failures stay within the owned session. Explicitly scoped model, shared pool, account, and provider gates remain active until expiry or newer valid evidence for the originating route. Historical records remain intact. Read-only readiness diagnostics use the same success eligibility and scope keys and preserve historical as-of decisions. Missing scope is displayed as `UNKNOWN`.

A standalone synthetic `delegate_task` with `opencode_recheck:true` and the exact native Free model can use one fixed marker prompt for a suspicious unscoped native 429. The scheduler checks single-flight ownership and persists the probe attempt before dispatch. An active explicitly scoped gate prevents a probe. A failed probe cannot trigger another attempt inside the same gate. Normal routing and capability listing do not initiate probes.

The live acceptance imported the original record into an isolated state. Before the call the route was `RATE_LIMITED` with `COOLDOWN_SUSPECT` and the original retry deadline. Exactly one native attempt on the same pinned anonymous route returned the fixed marker, matching actual model identity, guard and terminal evidence, and cost 0. Response SHA-256 was `46c1b5ebd6ba071ff48e8f7cde216c1043f4824526d50dbce574a2aeec25de8c`. After reopening the same SQLite state the route remained `HEALTHY` and `RECOVERED`. Its utility preview returned `AVAILABLE_CANDIDATE`. The OpenCode unavailable warning was absent and numerical Free capacity remained unknown.

Retry diagnostics retain validated relative and absolute hints and bounded invalid numeric inputs. HTTP dates use a sufficiently close server Date when present. Long transient backoff is capped separately from quota evidence. No reset is asserted for a relative delay alone.

The later OpenCode native background-child acceptance received a new 429 at 17:17:20 UTC with an unscoped body delay of 24,160,000 ms. The effective retry deadline was approximately 2026-11-11 00:00 UTC. This is a separate provider observation after the successful recovery and is authoritative for the latest attempt. No further OpenCode call was sent in this execution. The normalized capacity view uses `retry_at` for backoff and reserves `reset_at` for reported absolute quota reset evidence.

## Deterministic acceptance

| Cases | Coverage |
| --- | --- |
| OC-01–OC-03, OC-10, OC-14 | Same-route success supersedes an older gate. Different account, endpoint, desktop transport, or binary leaves it intact. A newer valid failure wins over out-of-order evidence. |
| OC-04–OC-05, OC-08–OC-09 | Expiry, asserted scope, unrelated models, and durable single-flight probe admission. |
| OC-06–OC-07, OC-15 | Wrapped upstream 429, milliseconds, seconds, HTTP dates with skew, absolute timestamps, malformed values, and bounded transient backoff. |
| OC-11–OC-13 | Legacy SQLite attempts, restart replay, readiness as-of behavior, AGY reserve routing, and accurate OpenCode warnings. |
