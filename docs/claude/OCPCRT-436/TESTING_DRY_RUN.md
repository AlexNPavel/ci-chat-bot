# Testing the Request Command with Dry-Run Mode

This guide shows you how to safely test the `request` command using dry-run mode, which allows you to test the full functionality without modifying IAM policies.

## Quick Start

### 1. Enable Dry-Run Mode

Add this environment variable to your deployment:

```yaml
env:
  - name: GCP_ACCESS_DRY_RUN
    value: "true"
  - name: GCP_SERVICE_ACCOUNT_JSON
    valueFrom:
      secretKeyRef:
        name: gcp-access-credentials
        key: service-account.json
  - name: ORG_DATA_BUCKET
    value: "your-org-data-bucket"
```

Or if running locally:
```bash
export GCP_ACCESS_DRY_RUN=true
export GCP_SERVICE_ACCOUNT_JSON='...'
export ORG_DATA_BUCKET='your-org-data-bucket'
```

### 2. Deploy and Test

1. Deploy the bot with dry-run enabled
2. Check logs to confirm dry-run mode:
   ```bash
   kubectl logs -n ci deployment/ci-chat-bot | grep "DRY-RUN"
   ```
   You should see: `GCP credentials manager running in DRY-RUN mode - IAM changes will be skipped`

3. Send command in Slack:
   ```
   request gcp-access "Testing dry-run mode"
   ```

### 3. Verify Dry-Run Behavior

**✅ What WILL happen:**
- ✅ Command parsing and validation
- ✅ Organization membership check
- ✅ Platform validation
- ✅ BigQuery audit log entry created
- ✅ User receives success message
- ✅ Logs show: `DRY-RUN: Would grant GCP IAM credentials to user...`

**❌ What WILL NOT happen:**
- ❌ No IAM policy changes
- ❌ No actual access granted
- ❌ No risk to production

### 4. Check BigQuery Logs

Query the audit logs to verify logging works:

```sql
SELECT
  timestamp,
  user_email,
  slack_user_id,
  justification,
  platform,
  expires_at
FROM `openshift-crt-ephemeral-access.ci_chat_bot.access_grants`
WHERE DATE(timestamp) = CURRENT_DATE()
ORDER BY timestamp DESC;
```

You should see your test entries!

## Testing Checklist

- [ ] Dry-run mode enabled (check logs)
- [ ] Command parsing works (try invalid inputs)
- [ ] Organization validation works (try invalid org)
- [ ] Platform validation works (try invalid resource)
- [ ] Success message returned to user
- [ ] BigQuery logs are created
- [ ] IAM policy unchanged (verify in GCP Console)
- [ ] Logs show "DRY-RUN" messages

## Test Cases

### Test 1: Valid Command
```
request gcp-access "Testing feature X"
```
**Expected:** Success message, BigQuery log created, no IAM changes

### Test 2: User Not in Hybrid Platforms
Test with a user who is not in the Hybrid Platforms organization.

**Expected:** Error message: "You are not a member of the 'Hybrid Platforms' organization..."

### Test 3: Invalid Platform
```
request aws "Testing"
```
**Expected:** Error message: "Currently, access is only available for the 'gcp-access' resource."

### Test 4: Missing Parameters
```
request
```
**Expected:** Usage error with example

### Test 5: Verify BigQuery Logging
1. Run valid command
2. Query BigQuery
3. Verify row exists with correct data

## Switching to Production Mode

Once testing is complete:

1. **Remove or set to false:**
   ```yaml
   env:
     - name: GCP_ACCESS_DRY_RUN
       value: "false"  # or remove this line entirely
   ```

2. **Redeploy:**
   ```bash
   kubectl rollout restart deployment/ci-chat-bot -n ci
   ```

3. **Verify production mode:**
   ```bash
   kubectl logs -n ci deployment/ci-chat-bot | grep "access manager"
   ```
   Should see: `GCP access manager enabled` (without "DRY-RUN MODE")

4. **Test with a real non-owner account**

## Troubleshooting

### Logs show "access manager disabled"
- Check `GCP_SERVICE_ACCOUNT_JSON` is set
- Verify service account JSON is valid

### BigQuery logs not appearing
- Check service account has `roles/bigquery.dataEditor`
- Verify dataset and table exist
- Check for BigQuery errors in logs

### Command not responding
- Check bot is running: `kubectl get pods -n ci`
- Check logs for errors: `kubectl logs -n ci deployment/ci-chat-bot`
- Verify Slack bot token is valid

## Example Log Output

When dry-run mode is working correctly, you'll see:

```
I0112 15:30:00 gcp_access.go:136] GCP access manager running in DRY-RUN mode - IAM changes will be skipped
I0112 15:30:00 main.go:366] GCP access manager enabled (DRY-RUN MODE)
I0112 15:30:05 gcp_access.go:200] DRY-RUN: Would grant GCP IAM credentials to user test@example.com (role: roles/viewer, expires: 2026-01-19T15:30:05Z)
I0112 15:30:05 gcp_access.go:164] Logged credential grant to BigQuery: user=test@example.com, org=openshift, resource=gcp-access
I0112 15:30:05 gcp_access.go:254] DRY-RUN: Granted GCP IAM credentials to user test@example.com, expires at 2026-01-19 15:30:05 UTC. Justification: Testing feature X
```

## Next Steps

After successful dry-run testing:

1. Disable dry-run mode
2. Test with a real user account (not a project owner)
3. Verify IAM bindings are created correctly
4. Monitor BigQuery logs in production
5. Set up alerting for failed credential grants
