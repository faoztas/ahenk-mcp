# Contributing to Ahenk MCP

Use English for project-maintained documentation, prompts, comments, and messages. Preserve MCP names, schemas, error codes, CLI commands, and SQLite compatibility. For changes to routing, snapshots, or artifacts, run the offline regression suite from the README and include results in your pull request.

Use disposable Git repositories and fixture providers for tests. If a change requires a live provider check, document the exact model, transport, policy gate, and redacted outcome. Keep raw prompts, responses, credentials, account bindings, diffs, and local state private.

For an Intel macOS binary release, use `make dist` and review the archive's license and third-party files. Review the pinned dependency licenses when dependencies change. See the [license inventory](docs/third-party-licenses.md).
