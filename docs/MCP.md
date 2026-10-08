# Cluster Bot MCP integration

Cluster Bot embeds an authenticated Streamable HTTP MCP endpoint at
`https://ci-chat-bot-ci.apps.ci.l2s4.p1.openshiftapps.com/mcp`. It manages ordinary
Prow launch clusters through the same manager as Slack. ROSA, MCE, workflows,
standalone builds/tests, and updating a running cluster are outside this interface.

## Configuration and rollout

MCP is disabled when `--mcp-token-file` is omitted; `/mcp` returns 404. To enable
it, mount a nonempty bearer token file and configure both flags:

```text
--mcp-token-file=/etc/mcp/token
--mcp-public-url=https://ci-chat-bot-ci.apps.ci.l2s4.p1.openshiftapps.com/mcp
```

The public URL must use HTTPS. Tokens are loaded by the existing secret agent,
which watches the file for rotation. Mount the Secret as a volume rather than a
`subPath` file so Kubernetes can propagate updates. Do not put tokens in command
arguments, source control, logs, or connection examples. The service principal
for this integration is server-controlled `chai`.

Keep one active manager instance. Configure the companion deployment with one
replica and a `Recreate` update strategy to avoid overlapping managers; admission
uses a process-local lock. The deployment is maintained outside this checkout.
Its change must mount the token Secret, supply the flags, and confirm HTTPS
reachability from Chai's GKE environment before enabling the connector.

Slack permissions must cover `users:read`, `users:read.email`, `im:write`, and
`chat:write`, in addition to the bot's existing file and credential delivery
permissions. Identity lookup requires an active human account in the bot's
workspace with a usable `@redhat.com` email address. Cluster Bot derives the
Prow RBAC username from that verified email and obtains a DM channel before
submitting a new launch. Slack lookup or DM creation failure prevents submission.

Every HTTP request requires `Authorization: Bearer <token>`. Requests without an
`Origin` header are accepted for service clients; supplied origins must match
the configured public origin. Request bodies are limited to 1 MiB. The endpoint
uses the official Go MCP SDK v1.8.0 with stateless Streamable HTTP and JSON
responses; legacy SSE and an OAuth authorization server are outside this setup.
See the [SDK transport documentation](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/docs/protocol.md).

## Chai connection contract

Chai is trusted to supply the initiating user's `slack_user_id` from its
conversation context. Slack profile verification establishes that an account
exists; it cannot establish who initiated the Chai request. The connector must
prevent conversational input from selecting another user. Never infer ownership
from a job ID or MCP client metadata.

Use MCP discovery to obtain the input and output schemas. Every tool requires
`slack_user_id`:

| Tool | Other arguments | Result |
| --- | --- | --- |
| `launch_cluster` | `request_id`, `inputs`; optional `platform`, `architecture`, `parameters` | Accepted/current cluster and `replayed` |
| `get_cluster_status` | `job_id` | Summary and lifecycle state |
| `get_cluster_credentials` | `job_id` | Sensitive access details |
| `list_clusters` | None | User's active ordinary Prow clusters |
| `destroy_cluster` | `job_id` | Job ID and termination state |

For example, the arguments to `launch_cluster` may be:

```json
{
  "slack_user_id": "U0123456789",
  "request_id": "conversation-42-launch-1",
  "inputs": ["4.23", "openshift/installer#1234"],
  "platform": "aws",
  "architecture": "amd64",
  "parameters": {"no-spot": ""}
}
```

The example user and PR are placeholders. `inputs` is one existing launch input
group: an explicit valid version or release image, optionally with PRs. Omitted
platform and architecture use Slack launch defaults, including HyperShift
selection rules. Flag-style parameters use an empty value. Unknown parameters,
unsupported combinations, and `test` are rejected. Available combinations depend
on the configured Prow jobs; the supported-platform list is not an availability
guarantee. Ownership, channels, job names, backend credentials, and lifetime
overrides cannot be supplied by the caller.

Successful results are under `data`. Domain failures set MCP `isError: true`
and return `error` with `code`, `message`, `retryable`, and optionally `job_id`.
Unknown jobs and jobs owned by somebody else both return `NOT_FOUND`.

| Error code | Caller action |
| --- | --- |
| `INVALID_ARGUMENTS` | Correct the launch inputs or tool arguments |
| `INVALID_IDENTITY` | Supply the initiating active workspace user's ID |
| `ACTIVE_CLUSTER_EXISTS` | Use or terminate the user's existing cluster |
| `CAPACITY_EXHAUSTED` | Wait for capacity and retry |
| `REQUEST_CONFLICT` | Restore the original arguments; use a new ID only for a new intended launch |
| `LAUNCH_INITIALIZATION` | Retry the identical request with the same ID |
| `NOT_FOUND` | Check the job ID, ownership, and ProwJob retention |
| `CREDENTIALS_UNAVAILABLE` | Poll while provisioning or recovering; terminal jobs cannot provide access |
| `BACKEND_UNAVAILABLE` | Retry when the dependency is available |
| `INTERNAL_ERROR` | Investigate the failure before starting a new launch |

Honor the returned `retryable` value rather than assuming every instance of a
code is transient. For launch failures, preserve the original request ID across
retries even when the result has no logs URL.

## Retries and asynchronous behavior

Generate a stable `request_id` for each intended launch and persist it before
calling the tool. Reuse that ID and identical arguments after a timeout, lost
response, retryable initialization error, or bot restart. A retained matching
ProwJob returns the original job, including terminal results, with `replayed:
true`. Changing arguments while reusing the ID yields `REQUEST_CONFLICT`.
The fingerprint records requested arguments before resolving mutable aliases
such as `nightly`; a retry recovers the original launch.

Deduplication is scoped to service principal, Slack user, and request ID, and
lasts only while the ProwJob is retained. The checked deployment uses **24-hour
ProwJob retention**. Do not replay an old launch after retention expecting
permanent deduplication. Retry ambiguous submissions with the same ID; allocating
a fresh ID may create another cluster.

Launch submission is bounded to 60 seconds and returns after durable ProwJob
acceptance. Provisioning and notifications continue independently of the HTTP
connection; a logs URL may be absent initially. Cluster Bot sends the initiating
user completion or failure notifications in their DM. Replays do not initiate a
new launch or DM. Existing Slack notification delivery semantics apply.

| State | Meaning |
| --- | --- |
| `provisioning` | Launch is underway; credentials are not ready |
| `ready` | Existing monitor obtained credentials and completed readiness checks |
| `failed` | Launch failed |
| `terminating` | Shutdown was requested; infrastructure cleanup is asynchronous |
| `terminated` | Launch job has ended or was cancelled |
| `expired` | Cluster lifetime has elapsed |

A successful terminal ProwJob indicates the launch job has ended, not that the
cluster has just become ready. Terminal conditions and expiry override cached
credentials. During restart recovery, credentials remain unavailable until the
monitor reconstructs them.

Credentials are available only from `get_cluster_credentials` for the verified
owner's ready cluster. Treat kubeconfigs and console passwords as secrets: do
not log tool bodies, cache them in shared transcripts, or expose them to other
users. Summaries, listing, and error responses contain no credentials. Metal
clusters retain their existing [proxy access requirements](FAQ.md).

Destruction targets the exact owned job ID, including clusters created through
Slack. Repeated calls for a retained job are safe. The result confirms the
cancellation request; it does not confirm cloud resources have been reclaimed.
An old job ID never targets a replacement cluster. Ordinary lifetime limits and
automatic cleanup still apply.

## Integration acceptance and rollback

Before production enablement, verify the following against a local/dev deployment
and Chai connector:

1. Launch for user A; confirm the stable job ID, completion DM, and working access.
2. Confirm Slack and MCP enforce the same active-cluster limit.
3. Confirm user B cannot inspect, retrieve credentials for, or destroy A's job.
4. Lose a launch response and restart the bot; retry the same request ID and
   confirm recovery of the original job without another submission.
5. Destroy the specified cluster and observe asynchronous cleanup; separately
   verify an untouched launch expires and is cleaned up automatically.
6. Confirm Chai obtains the user ID from trusted conversation context and
   preserves the request ID across retries.

Operation count and duration metrics use bounded tool and outcome labels. Audit
events include principal, Slack user ID, job ID, request-key hash, operation, and
outcome, without tokens or access secrets.

Rollback by omitting `--mcp-token-file` or revoking/rotating the token. Previously
accepted jobs continue through the manager, Slack controls, and automatic cleanup.
