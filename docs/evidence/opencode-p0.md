# OpenCode Free P0 evidence

## G-005 — Cooldown and live recovery

Two historical SQLite records for anonymous `opencode/deepseek-v4.1-flash-free` preserved normalized `RATE_LIMIT` and retry durations. The older schema did not capture the HTTP response or exact upstream scope. `import-opencode-cooldown` opened each source read-only, preserved provenance, and applied the later expiration in the pilot state. The original jobs stayed in their source databases.

After the imported cooldown elapsed, Codex App started one read-only synthetic attempt on DeepSeek. Later requests evaluated LongCat, then Big Pickle with a bounded GLM fallback. Each attempt received local HTTP 502 wrapping upstream 429, with a `provider_body` retry value and unknown upstream scope. Read-only verification confirmed terminal `BLOCKED / RATE_LIMIT` records, separate cooldowns, cleanup, and unchanged synthetic sources. The failed attempts supplied no result artifact or runtime cost evidence.

The stateless text endpoint accepts an exact model and returns text. Model echo and token usage are `NOT_EXPOSED`. Ahenk sent one local production request per attempt. The pinned OpenCode build can make up to four gateway requests for selected retryable failures. The actual gateway request count was unavailable. [OpenCode generation](https://github.com/anomalyco/opencode/blob/v2.2.6/packages/core/src/generate.ts) · [OpenCode retry policy](https://github.com/anomalyco/opencode/blob/v2.2.6/packages/core/src/model-retry.ts)

### Native and desktop diagnostics

The desktop UI and a replay through its running service returned GLM results. Separate isolated and desktop-service sessions with all tools disabled received 429 responses. The pinned binary and exact model configuration matched across the comparisons. The evidence did not establish what caused the different outcomes.

Offline checks against a local test double showed that `steps=1` can disable tool selection before the first generation. `steps=2` avoided that limit. Setting every tool to `ask` did not prevent an injected `execute` call from changing the test session title. Denying all actions except `read` configured to ask exposed exactly one tool. A denied in-scope read and an out-of-scope read sent no file content. Injected shell, write, JavaScript, sub-agent, network, and unknown MCP calls produced no synthetic side effects. These checks used no live model. [OpenCode permissions](https://opencode.ai/docs/permissions/) · [OpenCode tools](https://opencode.ai/docs/tools/)

A separate live native GLM pilot with `steps=2` and only `read` configured to ask returned the exact marker without a tool event. The subsequent Ahenk-integrated native smoke and M1 receiver provided the final acceptance evidence. [Native OpenCode acceptance](native-opencode.md) · [M1 acceptance](m1-build.md)

## G-006 — Catalog and limit scope

`ahenk probe-opencode-catalog` queried the pinned Intel OpenCode 2.2.6 binary's loopback `GET /api/model` endpoint and found 10 distinct anonymous chat models with normalized zero pricing. A normalized cache-write zero can represent an omitted upstream value. The catalog exposed neither remaining requests nor quota pool identity.

OpenCode's gateway uses a daily IP key for anonymous access and can add a model-specific prefix for explicit model rate limits. Both cases can produce the same 429 shape and midnight UTC cooldown. Ahenk retains legacy local pool labels for provenance and applies ambiguous cooldowns to the exact route. An explicitly asserted `pool` or `account` scope applies across the indicated routes. Numeric capacity remains `UNKNOWN` without measurement. [Gateway rate-limit handling](https://github.com/anomalyco/opencode/blob/v2/packages/console/app/src/routes/zen/util/handler.ts) · [Anonymous IP limiter](https://github.com/anomalyco/opencode/blob/v2/packages/console/app/src/routes/zen/util/ipRateLimiter.ts) · [Upstream issue](https://github.com/anomalyco/opencode/issues/59387)

### Historical public catalog

The 10 November 2026 machine catalog contained 12 non-deprecated free chat models. It reported zero input, output, and cache-read prices for each. Only Big Pickle and Inkling 2 Small Free had explicit zero cache-write pricing. Nemotron 3 Ultra Free appeared in the Console but was deprecated in the machine catalog. Jev 1.13 Free belonged to the separate decision endpoint. These historical observations do not attest current access or quota. [OpenCode machine catalog](https://models.opencode.ai/experimental/v2.0/api.json?type=chat,decision,image) · [Console model list](https://opencode.ai/docs/console/models)

| Exact model | Availability in the audited 2.2.6 catalog |
| --- | --- |
| `opencode/beam-free` | Built in |
| `opencode/big-pickle` | Built in |
| `opencode/deepseek-v4.1-flash-free` | Built in |
| `opencode/flint-40b-free` | Built in |
| `opencode/glm-5.3-flash-free` | Built in |
| `opencode/iceman-free` | Public machine catalog only |
| `opencode/inkling-2-small-free` | Public machine catalog only |
| `opencode/longcat-2.5-free` | Verified override |
| `opencode/mimo-v2.6-flash-free` | Built in |
| `opencode/minimax-m3.1-flash-free` | Public machine catalog only |
| `opencode/nemotron-3.5-lightning-free` | Built in |
| `opencode/nemotron-3.5-super-vl-free` | Built in |

LongCat was absent from the default local catalog. The read-only probe supplied a verified `OPENCODE_MODELS_PATH` entry and confirmed normalized zero pricing. The public source listed cache-write pricing as `NOT_PRICED`. The Console reported a 100-request daily limit for LongCat, which does not reveal remaining capacity. [Console free models](https://opencode.ai/docs/console/models#free-models) · [Models.dev](https://models.dev/)
