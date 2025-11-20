# Request Command Implementation

This document describes the implementation of the `request` command for the ci-chat-bot, which allows users to request access to a GCP workspace via IAM policy bindings.

## Overview

The `request` command adds users as IAM members of a GCP project with time-based conditional access. Access is granted for 7 days and automatically expires via IAM policy conditions. Users cannot extend their access; they must wait for expiration before requesting new access.

## Features

- ✅ Parameter-based command interface: `request <resource> "<justification>"`
- ✅ Self-service revocation: `revoke <resource>`
- ✅ Automatic 7-day expiration via IAM conditions
- ✅ Automatic cleanup of expired users (runs hourly)
- ✅ Runtime caching with GCP IAM as source of truth
- ✅ Group-based access validation using cyborg-data (Hybrid Platforms membership required)
- ✅ Business justification tracking in IAM condition descriptions
- ✅ Resource validation (currently supports: gcp-access resource only)
- ✅ Integration with Google Cloud Resource Manager API for IAM policy management
- ✅ **BigQuery audit logging** - All successful credential grants are logged to BigQuery for audit purposes

## Files Created/Modified

### New Files

1. **`pkg/manager/gcp_access.go`**
   - `GCPAccessManager` struct with runtime cache and BigQuery client
   - Google Cloud Resource Manager API integration using `googleapi.Error` for proper error handling
   - BigQuery integration for audit logging
   - GCP IAM policy as the source of truth (no ConfigMap needed)
   - Methods: `GrantAccess()`, `RevokeAccess()`, `CleanupExpiredAccess()`, `GetUserGrant()`, `logAccessGrant()`
   - IAM policy management with time-based conditions for automatic expiration
   - Helper functions: `parseExpirationFromCondition()`, `extractEmailFromMember()`
   - Error checking functions using HTTP status codes instead of string matching
   - Each user gets a unique IAM binding with an expiration condition
   - `AccessGrantLogEntry` struct for BigQuery audit log schema

### Modified Files

1. **`pkg/manager/types.go`**
   - Added `gcpAccessManager` field to `jobManager` struct
   - Added `orgDataService` field to `jobManager` struct for organizational data validation
   - Added `OrgDataService` interface for group membership validation
   - Added interface methods: `GrantGCPAccess()`, `RevokeGCPAccess()`, `GetGCPAccessManager()`, `GetOrgDataService()`

2. **`pkg/manager/manager.go`**
   - Updated `NewJobManager()` to accept `GCPAccessManager` and `OrgDataService` parameters
   - Added `GrantGCPAccess()` implementation
   - Added `RevokeGCPAccess()` implementation
   - Added `GetOrgDataService()` implementation
   - Added `gcpAccessCleanup()` background job (runs hourly)
   - Updated `Start()` to run cleanup goroutine

3. **`pkg/slack/actions.go`**
   - Added `Request()` command handler for granting access
   - Added `Revoke()` command handler for revoking access
   - Validates user membership in Hybrid Platforms organization using cyborg-data
   - Provides user-friendly response messages

4. **`pkg/slack/slack.go`**
   - Registered `request <resource?> <justification>` command in `SupportedCommands()`
   - Registered `revoke <resource?>` command in `SupportedCommands()`

5. **`pkg/slack/events/messages/message_handler.go`**
   - Added `request` command to help overview
   - Added detailed help in `GenerateManageHelpMessage()`

6. **`cmd/ci-chat-bot/main.go`**
   - Initialize cyborg-data service for organizational data from GCS
   - Initialize GCP access manager from environment variables
   - Pass both services to `NewJobManager()`

## Configuration

### Environment Variables

The following environment variables must be set for the `request` command to work:

1. **`ORG_DATA_BUCKET`** (required)
   - GCS bucket name containing organizational data
   - Example: `my-org-data-bucket`
   - Used to load employee and team membership information from cyborg-data

2. **`ORG_DATA_OBJECT_PATH`** (optional)
   - Path to the org data JSON file within the bucket
   - Defaults to `orgdata/comprehensive_index_dump.json` if not specified
   - Example: `production/org_data.json`

3. **`GCP_SERVICE_ACCOUNT_JSON`** (required)
   - JSON content of the GCP service account key
   - This credential is shared between two features:
     - **GCP Access Manager**: Modifies IAM policies for temporary access grants
     - **Organizational Data GCS**: Reads user/group membership data from GCS bucket
   - Service account must have permissions to:
     - Modify IAM policies on the target project (`roles/resourcemanager.projectIamAdmin` or similar)
     - Read objects from the GCS bucket specified in `ORG_DATA_BUCKET` (`roles/storage.objectViewer` or similar)
     - Insert rows into BigQuery for audit logging (`roles/bigquery.dataEditor` on the dataset)
   - Required scopes:
     - `https://www.googleapis.com/auth/cloud-resource` (for IAM policy management)
     - `https://www.googleapis.com/auth/bigquery` (for audit logging)
     - `https://www.googleapis.com/auth/devstorage.read_only` (for GCS access, included in cloud-resource scope)

4. **`GCP_ACCESS_DRY_RUN`** (optional)
   - Set to `"true"` to enable dry-run mode
   - When enabled, the bot will skip all IAM policy changes but still log to BigQuery
   - Useful for testing without affecting production IAM policies
   - Default: `false` (disabled)

### GCP Project Configuration

The following constants are configured in `pkg/manager/gcp_access.go`:

```go
const (
    // GCP project ID where users need IAM access
    GCPProjectID = "openshift-crt-ephemeral-access"

    // IAM role to grant to users for GCP workspace access
    GCPIAMRole = "roles/viewer"

    // Duration for which access is valid (7 days)
    GCPAccessDuration = 7 * 24 * time.Hour

    // Duration before automatic resource cleanup (48 hours)
    GCPResourceCleanupDuration = 48 * time.Hour

    // BigQuery dataset and table for audit logging
    BigQueryDataset = "ci_chat_bot"
    BigQueryTable   = "access_grants"
)
```

**Configuration:**
- **GCPProjectID**: Set to `"openshift-crt-ephemeral-access"` - the project where users will be granted temporary access
- **GCPIAMRole**: Set to `"roles/viewer"` - can be customized if a different permission level is needed (e.g., `roles/editor`, custom roles, etc.)
- **GCPAccessDuration**: Access duration of 7 days
- **GCPResourceCleanupDuration**: Resources older than 48 hours will be automatically deleted
- **BigQueryDataset**: Dataset name for audit logs (`"ci_chat_bot"`)
- **BigQueryTable**: Table name for audit logs (`"access_grants"`)

### Data Storage

The implementation uses **GCP IAM policy** as the single source of truth:

- IAM conditional bindings store all grant metadata (expiration timestamp in condition expression)
- Runtime cache improves performance by avoiding repeated API calls
- Cache is automatically populated from IAM policy when queried
- No Kubernetes ConfigMap needed

### BigQuery Audit Logging

All successful credential grants are logged to BigQuery for audit and compliance purposes:

**Table Schema (`ci_chat_bot.access_grants`):**
```sql
CREATE TABLE `openshift-crt-ephemeral-access.ci_chat_bot.access_grants` (
  timestamp TIMESTAMP,
  command STRING,
  user_email STRING,
  slack_user_id STRING,
  justification STRING,
  resource STRING,
  project_id STRING,
  expires_at TIMESTAMP
);
```

**Log Entry Fields:**
- `timestamp`: When the credential was granted (UTC)
- `command`: Always "request"
- `user_email`: Email address of the user granted access
- `slack_user_id`: Slack user ID who made the request
- `justification`: Business justification provided by the user
- `resource`: Resource name (e.g., "gcp-access")
- `project_id`: GCP project ID where access was granted
- `expires_at`: When the access will expire (UTC)

**Behavior:**
- Logging is **non-blocking** - failures to log will not prevent credential grants
- If BigQuery client fails to initialize, credential grants still work but won't be logged
- Errors are logged to klog for monitoring

## Setting Up GCP Service Account

### 1. Create Service Account

```bash
gcloud iam service-accounts create ci-chat-bot-gcp-access-creds \
    --display-name="CI Chat Bot GCP Credentials Manager" \
    --project=YOUR_PROJECT_ID
```

### 2. Grant IAM Permissions

Grant the service account permission to modify IAM policies on your target project:

```bash
gcloud projects add-iam-policy-binding YOUR_TARGET_PROJECT_ID \
    --member="serviceAccount:ci-chat-bot-gcp-access-creds@YOUR_PROJECT_ID.iam.gserviceaccount.com" \
    --role="roles/resourcemanager.projectIamAdmin"
```

**Note**: `roles/resourcemanager.projectIamAdmin` allows the service account to manage IAM policies. For production use, consider creating a custom role with minimal permissions:
- `resourcemanager.projects.getIamPolicy`
- `resourcemanager.projects.setIamPolicy`

### 2b. Grant BigQuery Permissions (for Audit Logging)

Grant the service account permission to insert data into BigQuery:

```bash
gcloud projects add-iam-policy-binding YOUR_TARGET_PROJECT_ID \
    --member="serviceAccount:ci-chat-bot-gcp-access-creds@YOUR_PROJECT_ID.iam.gserviceaccount.com" \
    --role="roles/bigquery.dataEditor"
```

**Note**: `roles/bigquery.dataEditor` allows the service account to insert rows into BigQuery tables. This is required for audit logging.

### 3. Create BigQuery Dataset and Table

Create the BigQuery dataset and table for audit logging:

```bash
# Create dataset
bq mk --dataset --location=US YOUR_TARGET_PROJECT_ID:ci_chat_bot

# Create table with schema
bq mk --table \
  YOUR_TARGET_PROJECT_ID:ci_chat_bot.access_grants \
  timestamp:TIMESTAMP,command:STRING,user_email:STRING,slack_user_id:STRING,justification:STRING,resource:STRING,project_id:STRING,expires_at:TIMESTAMP
```

### 4. Create and Download Key

```bash
gcloud iam service-accounts keys create ~/ci-chat-bot-sa-key.json \
    --iam-account=ci-chat-bot-gcp-access-creds@YOUR_PROJECT_ID.iam.gserviceaccount.com
```

### 5. Set Environment Variables in Deployment

Add to your Kubernetes deployment or secret:

```yaml
env:
  - name: GCP_SERVICE_ACCOUNT_JSON
    value: |
      {
        "type": "service_account",
        "project_id": "your-project",
        "private_key_id": "...",
        "private_key": "-----BEGIN PRIVATE KEY-----\n...",
        "client_email": "ci-chat-bot-gcp-access-creds@your-project.iam.gserviceaccount.com",
        "client_id": "...",
        "auth_uri": "https://accounts.google.com/o/oauth2/auth",
        "token_uri": "https://oauth2.googleapis.com/token",
        "auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
        "client_x509_cert_url": "..."
      }
```

Or use a Kubernetes Secret:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: gcp-access-credentials-config
  namespace: ci
type: Opaque
stringData:
  service-account.json: |
    {...}
```

Then reference in deployment:

```yaml
env:
  - name: GCP_SERVICE_ACCOUNT_JSON
    valueFrom:
      secretKeyRef:
        name: gcp-access-credentials-config
        key: service-account.json
```

## Usage

### For End Users

1. **Request access:**
   ```
   request gcp-access "Need to debug CI infrastructure issues"
   ```

   **Command format:** `request <resource> "<business justification>"`
   - `<resource>`: Resource name (currently only `gcp-access` is supported)
   - `"<business justification>"`: Brief explanation of why access is needed (must be enclosed in quotes)

   **Access Requirements:** You must be a member of the Hybrid Platforms organization to receive access. The bot automatically validates your membership before granting access.

2. **Response (first time):**
   ```
   Confirmed you are a member of the Hybrid Platforms organization. You have been added to project "openshift-crt-ephemeral-access".

   You will have access to the project for the next 7 days (until 2025-11-24 15:30 MST).

   You are responsible for deleting resources you create within this account, however, resources older than 48 hours will be deleted automatically.

   Use `gcloud auth application-default login` to make your credentials available for OpenShift installations & other programs requiring access to Google Cloud.

   Justification: Need to debug CI infrastructure issues
   ```

3. **Response (already has access):**
   ```
   You already have active access for project "openshift-crt-ephemeral-access".

   Your access will expire on 2025-11-24 15:30 MST (in 5 days).

   Original justification: Need to debug CI infrastructure issues
   ```

4. **Revoke access early:**
   ```
   revoke gcp-access
   ```

5. **Response (revoke successful):**
   ```
   Your GCP workspace access has been revoked successfully.
   ```

6. **Error responses:**
   - Missing parameters: "Invalid command format. Usage: request <resource> \"<business justification>\""
   - Wrong resource: "Currently, access is only available for the 'gcp-access' resource."
   - Not in Hybrid Platforms: "You are not a member of the 'Hybrid Platforms' organization. Access can only be granted to Hybrid Platforms members."

7. **Response (no active access):**
   ```
   You do not have any active GCP workspace access to revoke.
   ```

### Getting Help

```
help manage
```

Shows the `request` command in the management section.

## Architecture

### Data Flow

1. User sends `request <resource> "<justification>"` command to bot via Slack DM
2. Bot validates command parameters:
   - Checks that resource and justification are provided
   - Validates resource is "gcp-access" (only supported value currently)
3. Bot retrieves user's Slack ID and email from Slack profile
4. Bot validates user membership in Hybrid Platforms using cyborg-data:
   - If organizational data service is not available, returns error message
   - If user is not in Hybrid Platforms, returns error
5. Bot calls `GrantGCPAccess()` on JobManager with user's email, Slack ID, and justification
6. JobManager checks existing grants via `GetUserGrant()`:
   - If user has active access, returns expiration info including justification (no extension)
   - If new or expired, proceeds to grant
7. GCP Access Manager:
   - Checks runtime cache for existing grants
   - If exists and not expired, returns error "user already has active access"
   - If new or expired, creates a new IAM binding with a time-based condition
   - Condition includes title "Temp Access", expiration description with justification, and CEL expression
   - Expression format: `request.time < timestamp('2026-01-19T12:34:56Z')`
   - Description format: "Access expires on 2026-01-19T12:34:56Z. Justification: <user's justification>"
   - Updates runtime cache with grant details including justification
8. Returns success message with expiration and justification to user

### Background Cleanup

- Runs every hour via goroutine in `jobManager.Start()`
- Queries GCP IAM policy for all conditional bindings
- Parses condition expressions to extract expiration timestamps
- Identifies bindings with expiration times before current time
- Removes expired conditional bindings from IAM policy
- Updates runtime cache to remove expired entries
- **Note**: IAM conditions automatically deny access after expiration, but cleanup removes the bindings for hygiene

### Storage Schema

IAM policy binding structure (example for one user):

```json
{
  "role": "roles/viewer",
  "members": [
    "user:user1@redhat.com"
  ],
  "condition": {
    "title": "Temp Access",
    "description": "Access expires on 2026-01-19T18:30:00Z. Justification: Need to debug CI infrastructure issues",
    "expression": "request.time < timestamp('2026-01-19T18:30:00Z')"
  }
}
```

Runtime cache structure (in-memory):

```go
map[string]*UserAccessGrant{
  "user1@redhat.com": {
    Email:         "user1@redhat.com",
    GrantedAt:     time.Parse(...),  // Calculated from ExpiresAt - 7 days
    ExpiresAt:     time.Parse(...),  // Parsed from condition expression
    RequestedBy:   "U12345678",      // Slack user ID (only available at grant time, not from IAM)
    Justification: "Need to debug CI infrastructure issues",  // Business justification
  },
}
```

## Error Handling

The implementation handles several error scenarios using proper error type checking:

1. **Missing or invalid parameters**: Returns usage message with example command
2. **Invalid resource**: Returns error message indicating only "gcp-access" is currently supported
3. **Organizational data service not available**: Command returns error message asking user to contact administrator
4. **User not in Hybrid Platforms organization**: Returns error message: "You are not a member of the 'Hybrid Platforms' organization. Access can only be granted to Hybrid Platforms members."
5. **User not in specified organization**: Returns error message explaining user must be a member of the organization they specified
6. **Service account not configured**: Command returns helpful error message
7. **User email not found**: Returns error asking user to configure Slack profile
8. **Google API errors**: Uses `googleapi.Error` type with HTTP status codes for proper error detection
   - `isNotFoundError()`: Checks for HTTP 404 Not Found (instead of string matching)
9. **IAM policy modification errors**: Properly handles concurrent modifications and retries
10. **User already has access**: Returns informational message with expiration date and justification

## Testing

### Dry-Run Mode (Recommended for Testing)

To safely test the `request` command without modifying IAM policies, use dry-run mode:

1. **Enable dry-run mode:**
   ```bash
   # Set environment variable in your deployment
   export GCP_ACCESS_DRY_RUN=true

   # Or add to Kubernetes deployment:
   env:
     - name: GCP_ACCESS_DRY_RUN
       value: "true"
   ```

2. **Test the full flow:**
   ```
   request gcp-access "Testing the command"
   ```

3. **Verify dry-run behavior:**
   - Check pod logs for: `DRY-RUN: Would grant GCP IAM access to user...`
   - Confirm BigQuery audit logs are still being written
   - Verify IAM policy remains unchanged in GCP Console
   - User still receives success message

4. **Benefits of dry-run mode:**
   - Safe to test even if you're already a project owner
   - Full command flow is exercised (parsing, validation, org checks)
   - BigQuery logging works normally
   - No risk of modifying production IAM policies
   - Easy to enable/disable with environment variable

5. **When to use dry-run:**
   - Initial deployment testing
   - Testing with your own account
   - Verifying BigQuery integration
   - Testing command parsing and validation
   - Debugging without IAM side effects

### Manual Testing

1. **Test grant:**
   ```
   request gcp-access "Need to test new feature"
   ```
   Verify:
   - User added to project IAM policy with conditional binding
   - Binding description includes justification
   - Cache updated with justification

2. **Test already has access:**
   Run command again within 7 days
   Verify: User receives message showing current expiration date and original justification (no extension)

3. **Test missing parameters:**
   ```
   request
   ```
   Verify: User receives usage error with example


5. **Test invalid resource:**
   ```
   request "Hybrid Platforms" aws "Testing"
   ```
   Verify: User receives error "Currently, access is only available for the 'gcp-access' resource."

6. **Test revoke:**
   ```
   revoke gcp-access
   ```
   Verify: User removed from project IAM policy, cache cleared

7. **Test revoke without access:**
   Run `revoke gcp-access` without having active access
   Verify: User receives message "You do not have any active GCP workspace access to revoke."

8. **Test cleanup:**
   - Manually create an IAM binding with an expired condition timestamp
   - Wait up to 1 hour for cleanup job
   - Verify: User removed from project IAM policy, cache cleared

9. **Test error cases:**
   - User not in Hybrid Platforms organization (should be rejected immediately)
   - User validation only checks Hybrid Platforms membership
   - User without email in Slack profile
   - Invalid service account credentials
   - Organizational data service not available (ORG_DATA_BUCKET not set)

### Unit Tests

To add unit tests, create `pkg/manager/gcp_access_test.go`:

```go
package manager_test

import (
    "testing"
    "time"

    "github.com/openshift/ci-chat-bot/pkg/manager"
)

func TestGrantAccess(t *testing.T) {
    // Test implementation
}

func TestCleanupExpiredAccess(t *testing.T) {
    // Test implementation
}
```

## Security Considerations

1. **Service Account Key Protection**: Store in Kubernetes Secret, not in code
2. **Domain Validation**: Only @redhat.com emails allowed
3. **Automatic Expiration**: All access expires after 7 days via IAM condition expressions
4. **Condition-Based Access Control**: IAM conditions automatically deny access after expiration timestamp
5. **Audit Trail**: GCP IAM policy and Cloud Audit Logs provide full audit trail
6. **Least Privilege**: Service account only has IAM policy management permissions on target project
7. **Unique Bindings**: Each user gets a separate conditional binding for granular control
8. **No Persistent Storage**: No ConfigMap or database needed - IAM policy is the source of truth

## Monitoring

### Logs to Monitor

```bash
# Successful grants
grep "Granted GCP IAM access to user" logs

# Cleanup operations
grep "Running GCP.*cleanup" logs
grep "Removed expired GCP IAM access" logs

# Errors
grep "Failed to grant GCP" logs
grep "Failed to remove expired user" logs
```

### Metrics to Add (Future Enhancement)

Consider adding Prometheus metrics:
- `ci_chat_bot_gcp_access_grants_total` - Total grants
- `ci_chat_bot_gcp_access_active_users` - Current active users
- `ci_chat_bot_gcp_access_cleanup_runs_total` - Cleanup job runs
- `ci_chat_bot_gcp_access_errors_total` - Errors by type

## Troubleshooting

### Command Returns "Organizational data service is not available"

**Cause**: Cyborg-data not loaded or ORG_DATA_BUCKET not configured

**Fix**:
1. Check `ORG_DATA_BUCKET` environment variable is set
2. Check `ORG_DATA_OBJECT_PATH` if using custom path
3. Check pod logs for cyborg-data initialization errors
4. Verify GCS bucket exists and is accessible
5. Verify service account has Storage Object Viewer permissions on the bucket

### Command Returns "GCP access manager is not enabled"

**Cause**: Environment variables not set or service account configuration failed

**Fix**:
1. Check `GCP_SERVICE_ACCOUNT_JSON` is set
2. Check pod logs for initialization errors
3. Verify service account JSON is valid

### "Failed to set IAM policy: 403"

**Cause**: Service account lacks permissions to modify IAM policies

**Fix**:
1. Verify service account has `roles/resourcemanager.projectIamAdmin` role on the target project
2. Check that the service account credentials are correctly configured
3. Ensure the project ID is correct in the constants

### Users not being cleaned up after 7 days

**Cause**: Cleanup job not running or encountering errors

**Fix**:
1. Check pod logs for cleanup errors
2. Verify service account has `resourcemanager.projects.setIamPolicy` permission
3. Check Cloud Resource Manager API quota/rate limits
4. Note: Users are automatically denied access via IAM conditions, cleanup is just for hygiene

## Future Enhancements

1. **List command**: Show current access status and expiration
2. **Admin commands**: List all active grants, force revoke
3. **Notification**: Slack DM reminder 1 day before expiration
4. **Multiple roles**: Support different IAM roles for different purposes
5. **Audit logging**: Export grant/revoke events to external system
6. **Metrics dashboard**: Grafana dashboard for usage patterns
7. **Custom durations**: Allow admins to specify different expiration periods

## Dependencies

The implementation uses the following Go packages:

- `github.com/openshift-eng/cyborg-data/go` - Organizational data service for group membership validation
- `cloud.google.com/go/storage` - Google Cloud Storage client (for GCS data source with `-tags gcs`)
- `golang.org/x/oauth2` - OAuth2 authentication
- `google.golang.org/api/cloudresourcemanager/v1` - Google Cloud Resource Manager API
- `google.golang.org/api/googleapi` - Google API error types
- `k8s.io/client-go` - Kubernetes client for ConfigMap access
- `k8s.io/apimachinery/pkg/api/errors` - Kubernetes error utilities (aliased as `apierrors`)
- `k8s.io/klog` - Logging

### Build Tags

The application must be built with the `-tags gcs` build tag to enable GCS support for loading organizational data.

The Makefile has been updated to automatically include the `gcs` build tag:

```bash
make build
```

Or manually:

```bash
go build -tags gcs ./cmd/ci-chat-bot
```

Without this tag, the GCS data source will be a stub that returns an error.

## Organization Membership Validation

### Overview

The `request` command requires users to be members of the **Hybrid Platforms** organization. This is the only organization membership requirement - users do not need to specify an organization in the command.

### How It Works

1. **Automatic Hybrid Platforms Check**:
   - Every user requesting access MUST be a member of the "Hybrid Platforms" organization
   - The check happens automatically before granting access
   - If the user is not in Hybrid Platforms, they are rejected immediately with: "You are not a member of the 'Hybrid Platforms' organization. Access can only be granted to Hybrid Platforms members."
   - No organization parameter is required in the command - the validation always checks Hybrid Platforms membership

### Example Scenarios

**Scenario 1: User in Hybrid Platforms**
- Command: `request gcp-access "Need access"`
- User is in: Hybrid Platforms
- Result: ✅ Approved - Access granted

**Scenario 2: User not in Hybrid Platforms**
- Command: `request gcp-access "Need access"`
- User is NOT in: Hybrid Platforms
- Result: ❌ Rejected - "You are not a member of the 'Hybrid Platforms' organization..."

### Implementation Details

The membership check is implemented in [pkg/slack/actions.go](../../../pkg/slack/actions.go) in the `Request()` function:

```go
// Verify user is a member of Hybrid Platforms (required for all access)
if !isUserInOrg(orgDataService, event.User, email, "Hybrid Platforms") {
    return "You are not a member of the 'Hybrid Platforms' organization. Access can only be granted to Hybrid Platforms members."
}
```

The `isUserInOrg()` helper function (in the same file) performs the actual validation using cyborg-data:
- First attempts Slack ID lookup: `orgDataService.IsSlackUserInOrg(slackID, org)`
- Falls back to email-based lookup if Slack ID fails: `GetEmployeeByEmail()` + `IsEmployeeInOrg()`

### Testing

Unit tests in [pkg/slack/actions_request_test.go](../../../pkg/slack/actions_request_test.go) verify:
- Users not in Hybrid Platforms are rejected
- Users in Hybrid Platforms are approved
- Both lookup methods work correctly (Slack ID + email fallback)

## Deployment Checklist

- [ ] Set up organizational data in GCS bucket
- [ ] Configure `ORG_DATA_BUCKET` and optionally `ORG_DATA_OBJECT_PATH` environment variables
- [ ] Ensure GCS bucket has proper permissions for the service account
- [ ] Verify `GCPProjectID` constant is set to `"openshift-crt-ephemeral-access"` in `pkg/manager/gcp_access.go`
- [ ] Verify `GCPIAMRole` constant is set appropriately (default: `roles/viewer`) in `pkg/manager/gcp_access.go`
- [ ] Verify `BigQueryDataset` and `BigQueryTable` constants are set correctly in `pkg/manager/gcp_access.go`
- [ ] Create GCP service account for the bot
- [ ] Grant service account `roles/resourcemanager.projectIamAdmin` on `openshift-crt-ephemeral-access` project
- [ ] Grant service account `roles/bigquery.dataEditor` on `openshift-crt-ephemeral-access` project (for audit logging)
- [ ] Create BigQuery dataset `ci_chat_bot` in `openshift-crt-ephemeral-access` project
- [ ] Create BigQuery table `access_grants` with the correct schema
- [ ] Create Kubernetes Secret with service account JSON
- [ ] Update deployment to include `GCP_SERVICE_ACCOUNT_JSON` environment variable
- [ ] Set up automatic resource cleanup for resources older than 48 hours in the GCP project
- [ ] Build ci-chat-bot using `make build` (automatically includes `-tags gcs`)
- [ ] Deploy updated ci-chat-bot
- [ ] Test with a real user account (in Hybrid Platforms org)
- [ ] Test with a user not in Hybrid Platforms org (should be denied)
- [ ] Verify IAM conditional bindings are created correctly in GCP Console
- [ ] Verify BigQuery audit logs are being written
- [ ] Verify success message includes all required information
- [ ] Monitor logs for errors
- [ ] Document process for team

## Support

For issues or questions:
- Check pod logs: `kubectl logs -n ci deployment/ci-chat-bot`
- Review IAM policy in Google Cloud Console: IAM & Admin > IAM > View by Principals
- Filter for conditional bindings with title "Temp Access"
- Check Cloud Audit Logs for IAM policy changes
- Query BigQuery audit logs:
  ```sql
  SELECT * FROM `openshift-crt-ephemeral-access.ci_chat_bot.access_grants`
  WHERE DATE(timestamp) = CURRENT_DATE()
  ORDER BY timestamp DESC;
  ```
- File an issue at https://github.com/openshift/ci-chat-bot/issues
