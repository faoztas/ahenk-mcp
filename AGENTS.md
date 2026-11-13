# Ahenk working rules

- Follow the user's applicable AGENTS.md instructions.
- Obtain user approval before modifying other repositories.
- Verify the actual Git repository and worktree roots before each write.
- Keep credentials unread, uncopied, and out of logs.
- Obtain user approval before signing in to a provider account.
- In Plan Mode, produce a plan only. Leave the code untouched.
- Use AGY Headless without the AGY SDK.
- For subagent execution, implementation, bounded fixes, and quota conservation, delegate tasks using the `delegate_task` (or `spawn_subagent`) tool. By default, `delegate_task` operates synchronously (`execution_mode: "wait"`) in an isolated Git worktree and returns verified diffs. For parallel subagents, use `execution_mode: "async"` followed by `wait_jobs`.
- Preserve the existing Daniiislo integration.
- Obtain user approval before creating any paid API access or provider subscription.
- Commit, push, deploy, or create a remote GitHub repository only when the user separately requests it.
