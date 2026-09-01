# POMA Grill error codes

Every grill tool returns an error envelope alongside its normal output:

```json
{
  "error": "human-readable message",
  "code": "too_many_jobs",
  "retryable": true,
  "retry_after_seconds": 30
}
```

`isError` is `true` on the MCP response. Branch on `code`, never on the text of `error`.

## Retryable

Retry the identical call after `retry_after_seconds` (default to ~30s if absent).

| Code | Meaning | What to do |
|---|---|---|
| `too_many_jobs` | Account is at its concurrent-job ceiling. The document was **not** ingested. | Wait, retry the same call, and hold off on new ingests until running jobs finish. |
| `transport_error` | Network failure reaching the API. | Retry with backoff. |
| `stream_error` | The status (SSE) stream dropped mid-job. | The job may still be running — prefer `grill_ingest_resume` with the known `job_id` over re-uploading. |
| `upstream_error` | 5xx from the POMA API. | Retry with backoff. A 4xx `upstream_error` is terminal. |

## Terminal

Do not retry. Fix the cause or abort and tell the user.

| Code | Meaning | What to do |
|---|---|---|
| `missing_token` | No credential resolved. | Set `POMA_API_KEY` on the server process, use the hosted endpoint's OAuth flow, or pass `token` on the call. |
| `auth_expired` | Credential rejected or expired. | Re-authenticate at [console.poma-ai.com](https://console.poma-ai.com). Say explicitly which key was rejected and which one is needed. |
| `payment_required` | Plan limit hit. | Upgrade at [console.poma-ai.com](https://console.poma-ai.com). |
| `forbidden` | Credential is valid but not authorized for this project or action. | Check the project with `grill_projects`; the key may be scoped to a different one. |
| `project_protected` | The target project blocks this operation. | Choose another project or lift the protection in the console. |
| `invalid_input` | Bad arguments — e.g. zero or two of `file_path`/`file_base64`/`url`, a path outside `GRILL_INGEST_ALLOWED_PREFIX`, or a payload over `GRILL_INGEST_MAX_BYTES`. | Fix the arguments. Never retry unchanged. |
| `parse_error` | The API response could not be decoded. | Report it; likely a server-side or version-skew bug. |
| `job_failed` | Ingestion ran and failed — usually an unsupported or corrupt file. | Report the message to the user. Re-uploading the same bytes will fail the same way. |

## Server-side limits

| Env var | Effect |
|---|---|
| `GRILL_INGEST_ALLOWED_PREFIX` | If set, `file_path` must resolve (after symlink evaluation) under this directory. Non-regular files are rejected. |
| `GRILL_INGEST_MAX_BYTES` | Max payload size. Unset means 512 MiB; `0` disables the limit. |
| `POMA_PROJECT_ID` | Default project for account-level API keys. |
| `POMA_API_BASE_URL` / `POMA_STATUS_API_BASE_URL` | Override the API and status-stream endpoints. |
