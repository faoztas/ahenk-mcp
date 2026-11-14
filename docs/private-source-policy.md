# Private source policy

Private repository work is opt-in. Use a dedicated local stdio instance with a single registered repository and a separate, already-created, canonical `0700` state directory outside the repository. Its configuration must be a regular, singly linked file owned by the current user, with mode `0600` or `0400`. The local operator controls this file and the trusted MCP client. Use a separate server and state for synthetic work.

## Review and pin the data

Review the exact committed text before approval. Approve only the code needed for the job. Credentials, customer data, financial records, production data, attachments and working-tree changes are outside this policy. Use exact repository-relative paths and SHA-256 digests of the bytes from Git HEAD. Do the same for the task objective and ordered acceptance criteria. The following helper prints only the digest. It makes no provider call.

```sh
ahenk private-brief-digest < reviewed-brief.json
git show HEAD:src/example.go | shasum -a 256
```

`reviewed-brief.json` contains the two task fields:

```json
{"objective":"Summarize the approved source.","acceptance":[{"id":"summary","description":"Report one behavior from the file."}]}
```

Add this policy to the repository entry in the private configuration. Replace the example expiration and digests with the approved values. The repository's existing `allowed_paths` and `providers` must also authorize the named file and route.

```json
{
  "private_source": {
    "owner_consent": true,
    "writer_isolation_verified": false,
    "allow_write": false,
    "require_independent_review": false,
    "expires_at": "2027-01-01T00:00:00Z",
    "files": {"src/example.go": "<file-sha256>"},
    "approved_brief_sha256": ["<brief-sha256>"],
    "routes": [
      {
        "provider": "opencode",
        "model": "opencode/glm-5.3-flash-free",
        "binary_sha256": "<audited-binary-sha256>",
        "transport": "native_read_ask",
        "expected_account": "ANONYMOUS",
        "data_handling_reviewed": true
      }
    ]
  }
}
```

Before setting `data_handling_reviewed`, verify the exact provider and model's current terms, storage and training behavior and your authorization to disclose the selected text. An audited binary and a zero-price catalog entry establish route and cost checks. They do not establish confidentiality or provider retention. When `expected_account` or `expected_account_sha256` is specified on a route, the provider capability must attest the matching account principal; unattested sessions fail closed with `ACCOUNT_ATTESTATION_REQUIRED` and mismatched principals fail closed with `ACCOUNT_MISMATCH`. AGY remains blocked for private source unless its worker session attests the expected account. A successful AGY quota observation does not bind the execution session.

Submit a standalone `delegate_task` with `data_class: "repository_internal"`, `mode: "read_only"`, the reviewed brief, and exact `allowed_paths`. Supply `preferred_provider` and `preferred_model` when a job must use a specific approved route. They are hard filters for private jobs. A private-only repository rejects synthetic/public relabeling, secret patterns (`SECRET_DETECTED`), and personal-data classifications (`PERSONAL_DATA_CLASSIFIED`). Write admission (`mode: "patch"`) remains blocked with `WRITER_ISOLATION_UNVERIFIED` unless `writer_isolation_verified: true`, `allow_write: true`, `require_independent_review: true` (both in `private_source` and repository `limits`), and registered `go_checks` are configured; when enabled, private patches require a `distinct_provider` batch that runs the patch in an isolated detached worktree, passes the named deterministic Go check, and obtains an `APPROVED` independent review bound to the same `artifact_sha256`. Completed private jobs bind `grant_digest`, `snapshot_id`, and `PRIVATE_GRANT_BOUND` in their verification result. Use `list_capabilities` with `repo_id` to inspect `repo_admission` gates and `local_runners` before dispatch. Each dispatch and automatic retry checks the grant and its expiry, file digests and brief; retry also requires the same snapshot identity. Quota and cost controls remain in force when controller capacity is low.

## Copies and expiry

Ahenk captures only the approved HEAD text, up to 32 KiB, in memory. It keeps the snapshot identity and job metadata, the approved brief and the returned summary in its private state. A worker can write temporary session data under the attempt directory. Ahenk removes the isolated OpenCode session subtree after confirmed worker shutdown. The scratch attempt and lease metadata can remain. Failed removal is treated as a cleanup failure and quarantines the attempt. An expired grant blocks further egress. It does not delete prior job records. Existing logical collection scrubs terminal payloads after seven days and removes job records after thirty days. SQLite pages, WAL files, backups and provider storage can outlive that collection. For local removal, close the owning server and dispose of its dedicated state and any backups according to your data handling policy. Provider deletion is governed by the provider. Keep config, state, transcripts and output out of Git and public issues.
