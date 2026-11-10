# M1 build and live acceptance evidence

## Build and connection

Independent Intel macOS builds from the same source tree produced identical SHA-256 digests. The server and read-only verifier were checked against that digest. The historical full Go suite, vet, and race checks passed. Offline tests rejected incorrect challenges, extra diff lines, leaked receiver answers, and missing or unexpired cooldown prerequisites. Fixture state could not authorize a live M1 request.

Codex App connected through `mcp_servers.ahenk` and discovered an active scheduler, the synthetic M1 repository policy, and both pinned provider routes. CodexBar verified the Codex and AGY account bindings separately. A second stdio process connected to the same state reported `state_busy`. [CodexBar evidence](codexbar-p0.md)

## Live route selection

Early stateless OpenCode attempts against DeepSeek, LongCat, Big Pickle, and GLM returned upstream 429 responses and recorded separate route cooldowns. No production result or runtime cost evidence was available for those attempts. The scheduler preserved the original cooldown records after separating model and transport scope. The native GLM smoke then passed in the same state and supplied evidence for that exact route. [OpenCode diagnostics](opencode-p0.md) · [Native OpenCode acceptance](native-opencode.md)

## Live batch acceptance

On 10 November 2026, the verifier generated one request with `m1-request`. Codex App submitted it in a single `delegate_batch` call. A read-only inspection verified one parent and two child jobs sharing the same snapshot.

| Role | Route | Verified result |
| --- | --- | --- |
| Parent | `distinct_provider` | `COMPLETED` with terminal audit evidence |
| Producer | AGY `gemini-3.8-flash-medium` | One completed attempt with native finish, usage, subscription quota, and a one-line diff |
| Receiver | `opencode/glm-5.3-flash-free` via `native_read_ask` | One completed attempt with `stop`, usage, zero reported cost, and the exact marker derived from the diff |

The AGY result recorded documented cost protection as `VERIFIED` and runtime credit protection as `UNKNOWN`. The diff added one line to the synthetic `m1-marker.txt` file. Independent hashing matched the producer artifact, the receiver handoff, and the receiver result. `verify-m1` checked the original request, dependency, diff scope, marker provenance, and audit records. It recorded `source_unchanged=true` and verified the OpenCode model and usage evidence. Both workers cleaned up. The source checkout, index, and HEAD stayed unchanged.

Readiness reported `MULTI_PROVIDER_LIVE_READY=true` with `LIVE_DIFF_HANDOFF_VERIFIED`. The live state lacked a fixture batch. A separate fixture profile returned `OFFLINE_CORE_READY=true` with `FIXTURE_DIFF_HANDOFF_VERIFIED`. These are historical M1 results. The public summary omits local run identifiers, state paths, and account observations.
