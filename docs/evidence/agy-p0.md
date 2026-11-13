# AGY P0 acceptance evidence

<a id="g-001--done"></a>
## G-001 — DONE

Ahenk records the tools advertised during initialization and sends the prompt after the model, working directory, agent, and permission mode match. The execution profile governs file and command scope, session ownership, native child activity, message targets, and denials.

Signed AGY 1.5.3 emits the native `finish` step separately from tool steps. The restricted synthetic profile accepts it for the owned session. An optional payload may contain only the string fields `summary` and `patch`. Nested values, extra fields, and duplicate keys are rejected. A `system_message` step is allowed when it carries no tool data. Out-of-profile tool events and a terminal `denied_actions` report fail the attempt. [Headless event protocol](https://antigravity.google/docs/cli/headless/)

`go test ./internal/providers -run TestAGY -count=1` covered tool inventory, native finish, out-of-scope file and command use, cross-job messaging, permission denial, and missing terminal usage. In the live run, the advertised tool inventory passed initialization and the native finish step was observed.

<a id="g-002--done"></a>
## G-002 — DONE

The Intel macOS pilot verified Google-signed AGY 1.5.3 and selected `gemini-3.8-flash-medium`. `ahenk probe-agy` observed available five-hour and weekly Gemini subscription windows through `/usage` in `SAME_NATIVE_CLI_CONTEXT` and returned `ALLOW_RESTRICTED_SYNTHETIC`.

Ahenk set `useG1Credits:false` and an empty `modelProvider`, disabled automatic updates, and excluded API key and application-default credential environment variables. The documented credit protection was `VERIFIED`. AGY 1.5.3 did not report its effective runtime credit setting, so that field remained `UNKNOWN`. [CLI reference](https://antigravity.google/docs/cli/reference/) · [CLI setup](https://antigravity.google/docs/cli/install/) · [Troubleshooting](https://antigravity.google/docs/cli/troubleshooting/)

<a id="g-003--done"></a>
## G-003 — DONE

A restricted synthetic job on 9 November 2026 completed in one attempt. The preflight verified the model, directory, agent, `request-review` mode, and subscription quota. The provider emitted native `finish` and terminal `result` events with structured output and token usage. An independent read-only check matched the result hash and confirmed that the synthetic source file, index, and HEAD stayed unchanged. This supplied `AGY_LIVE_READY` evidence.

Earlier diagnostics exposed a permission denial and then an unsupported native finish step reported as `INVALID_OUTPUT` and `AGY_STEP_UNSUPPORTED`. The successful run followed the change that accepts a scoped native finish step from the owned session. [M1 acceptance](m1-build.md)
