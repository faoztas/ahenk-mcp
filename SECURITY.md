# Security policy

Report suspected vulnerabilities through [GitHub private vulnerability reporting](https://github.com/faoztas/ahenk-mcp/security/advisories/new) if enabled. If private reporting is unavailable, ask the maintainer for a private channel before sharing details. Public issues may describe a sanitized symptom.

Include the Ahenk revision, affected provider and transport, and reproduction steps using a synthetic repository. Keep credentials, account identifiers, prompts, raw provider output, state databases, and private diffs out of reports. If a credential was exposed, revoke or rotate it through its issuer.

Run Ahenk as a local stdio service in a trusted project. Scope each repository to the files and providers it needs. Review returned diffs before applying them. The current live provider pilots are limited to synthetic workloads. Temporary workspaces and application policy checks do not provide general subprocess containment.

## Trusted-local execution

An operator can opt a synthetic repository into `trusted_local` and request the `trusted-local` profile explicitly. Ahenk gives pinned providers a private worktree and per-attempt permissions for scoped reads and native edits. Module and test files are protected from the native writer by the task policy and by post-execution verification. The verifier runs only registered Go checks, with a reduced environment and dependency downloads disabled.

The provider and Go subprocesses still run under the local user's OS permissions. The worktree separates Git metadata and the verifier checks scoped checkout contents, the source index, and the artifact before and after verification. Those checks cannot establish host-wide containment or detect every external side effect. Keep trusted-local limited to disposable repositories with trusted check code. Ahenk quarantines attempts when it cannot confirm process cleanup.
