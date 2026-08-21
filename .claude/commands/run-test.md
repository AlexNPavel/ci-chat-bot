---
description: Run a test instance of the ci-chat-bot
---

You are helping the user run a test instance of the ci-chat-bot. Follow these steps:

## Security Warning

**IMPORTANT - Security Notice**: This command will ask you to provide Slack credentials during setup.

- Do NOT share the chat transcript or logs containing these credentials with others
- Credentials will be visible in process listings (`ps aux`) while the bot is running
- The ngrok tunnel exposes your local bot instance to the internet - only use test/development Slack apps
- Logs at `/tmp/ci-chat-bot/bot.log` may contain sensitive information
- For production deployments, use proper secret management (Kubernetes secrets, vault, etc.) instead of environment variables
- **Process management**: this workflow tracks the bot and ngrok processes via PID files (`/tmp/ci-chat-bot/bot.pid`, `/tmp/ci-chat-bot/ngrok.pid`) so they can be stopped precisely. **Never** use broad-match kill commands (`pkill -f <generic substring>`, `killall`, `pkill node`, `pkill go`, `pkill ngrok`, etc.) in this workflow — a broad pattern can match unrelated processes, including the Claude Code CLI's own process tree, and kill it.

0. **Prepare the working directory**: Run `mkdir -p /tmp/ci-chat-bot` before starting anything. All logs and PID files for this workflow live under this one directory rather than scattered directly in `/tmp`.

1. **Check Environment Variables**: First ask the user if they want to load environment variables from a file.

   **Option A: Load from Environment File (Recommended)**

   Ask the user for the path to their environment file (e.g., `.env`, `.env.local`, etc.).

   The file should contain one variable per line in the format:
   ```bash
   BOT_TOKEN=xoxb-your-token-here
   BOT_SIGNING_SECRET=your-signing-secret
   GITHUB_TOKEN=ghp_your-github-token
   GCP_ACCESS_DRY_RUN=true
   GCP_SERVICE_ACCOUNT_JSON={"type":"service_account",...}
   ORG_DATA_BUCKET=your-org-data-bucket
   ```

   If the user provides a file path:
   - Verify the file exists
   - Load the environment variables using `source` or `export $(cat file | xargs)`
   - Store the file path to use in step 5

   **Option B: Manual Entry (if no env file)**

   If they do NOT have an env file or prefer manual entry, ask the user to provide:
   - `BOT_TOKEN`: Slack Bot Token (required) - starts with `xoxb-`
   - `BOT_SIGNING_SECRET`: Slack App Signing Secret (required)
   - `GITHUB_TOKEN`: GitHub token (optional but recommended)
   - `GCP_ACCESS_DRY_RUN`: Set to `true` to enable dry-run mode for GCP credentials (optional)
   - `GCP_SERVICE_ACCOUNT_JSON`: GCP service account JSON for credentials command (optional)
   - `ORG_DATA_BUCKET`: GCS bucket for organizational data (optional)

   Store these values to use in step 5. Tell the user where to find these values:
   - Go to https://api.slack.com/apps
   - Select their app
   - **BOT_TOKEN**: OAuth & Permissions → Bot User OAuth Token
   - **BOT_SIGNING_SECRET**: Basic Information → App Credentials → Signing Secret

   **About GCP_ACCESS_DRY_RUN**:
   - When set to `true`, the bot will skip all IAM policy changes for the `credentials` command
   - BigQuery audit logging will still work normally
   - Useful for testing the credentials command without affecting production IAM
   - Safe to use even if you're already a project owner
   - See TESTING_DRY_RUN.md for more details

2. **Verify Cluster Access**: Confirm the user has `oc` CLI access to the `app.ci` cluster context:
   - Run `oc --context app.ci whoami` to verify access
   - If this fails, the user needs to authenticate to the OpenShift CI cluster first

3. **Setup ngrok Tunnel**: Start ngrok to expose the bot to Slack:
   - Run ngrok in the background, capturing its output and PID so it can be managed later:
     ```bash
     ngrok http 8080 > /tmp/ci-chat-bot/ngrok.log 2>&1 &
     echo $! > /tmp/ci-chat-bot/ngrok.pid
     ```
   - Extract and display the public HTTPS URL that ngrok provides
   - The URL will look like: `https://xxxx-xx-xx-xx-xx.ngrok-free.app`
   - Inform the user they need to configure this URL in their Slack app settings:
     - Go to the Slack app configuration page (https://api.slack.com/apps)
     - Navigate to "Interactivity & Shortcuts"
       - Set Request URL to: `https://xxxx-xx-xx-xx-xx.ngrok-free.app/slack/interactive-endpoint`
     - Navigate to "Event Subscriptions"
       - Set Request URL to: `https://xxxx-xx-xx-xx-xx.ngrok-free.app/slack/events-endpoint`

4. **Build the Project**: Run `make` to build the ci-chat-bot binary.

5. **Run the Full Setup**: Execute the complete setup with log redirection.

   **If using an environment file (Option A from step 1):**
   ```bash
   set -a && source /path/to/.env && set +a && make run > /tmp/ci-chat-bot/bot.log 2>&1 &
   echo $! > /tmp/ci-chat-bot/bot.pid
   ```
   Replace `/path/to/.env` with the actual file path provided by the user.

   **If using manual entry (Option B from step 1):**

   Normal mode (with IAM changes):
   ```bash
   BOT_TOKEN=<token-from-step-1> BOT_SIGNING_SECRET=<secret-from-step-1> make run > /tmp/ci-chat-bot/bot.log 2>&1 &
   echo $! > /tmp/ci-chat-bot/bot.pid
   ```

   Dry-run mode (recommended for testing credentials command):
   ```bash
   GCP_ACCESS_DRY_RUN=true BOT_TOKEN=<token-from-step-1> BOT_SIGNING_SECRET=<secret-from-step-1> make run > /tmp/ci-chat-bot/bot.log 2>&1 &
   echo $! > /tmp/ci-chat-bot/bot.pid
   ```

   Use the actual values provided by the user in step 1.

   This will:
   - Extract kubeconfig files from the `ci-chat-bot-kubeconfigs` secret
   - Get Boskos credentials from the `boskos-credentials` secret
   - Extract ROSA configuration (subnet IDs, OIDC config ID, billing account ID)
   - Extract MCE kubeconfig and token
   - Build the binary if needed
   - Start the bot with all required configuration
   - Redirect all output to `/tmp/ci-chat-bot/bot.log` for easy monitoring
   - Record the backgrounded bot's PID to `/tmp/ci-chat-bot/bot.pid`

   Note: `make run` may itself spawn the actual bot binary as a child process, so `$!` (the PID of the `make run` shell) is a reasonable handle for stopping the tree, but always verify with `ps -p "$PID" -o cmd=` before killing (see "Relaunching the Bot" below).

6. **Verify the Bot is Running**:
   - Check that the bot starts without errors
   - By default it listens on port 8080
   - Verify ngrok is still running and forwarding requests
   - Monitor logs with: `tail -f /tmp/ci-chat-bot/bot.log`
   - Test basic Slack connectivity by sending a message to the bot in Slack

7. **Inform User About Log Monitoring**: After starting the bot, inform the user:
   - Logs are saved to `/tmp/ci-chat-bot/bot.log`
   - They can monitor logs in real-time with: `tail -f /tmp/ci-chat-bot/bot.log`
   - To filter for errors: `tail -f /tmp/ci-chat-bot/bot.log | grep -i error`
   - To filter for warnings: `tail -f /tmp/ci-chat-bot/bot.log | grep -i warning`

## Relaunching the Bot

"Relaunching" means restarting **only** the ci-chat-bot process. **Leave ngrok running** — its tunnel URL doesn't need to change across a relaunch, and the Slack app's configured Request URLs point at that tunnel, so tearing it down would break the Slack app config for no reason. The `/tmp/ci-chat-bot/ngrok.pid` file exists solely so ngrok can be shut down cleanly later, when the user explicitly says to stop testing — not as part of a relaunch.

1. **Stop the bot narrowly**, verifying the PID actually belongs to the bot before killing it:
   ```bash
   if [ -f /tmp/ci-chat-bot/bot.pid ]; then
     PID=$(cat /tmp/ci-chat-bot/bot.pid)
     if ps -p "$PID" -o cmd= | grep -q "ci-chat-bot\|make run"; then
       kill "$PID"
     else
       echo "PID $PID does not look like the ci-chat-bot process; not killing. Inspect manually."
     fi
   fi
   ```
   If `/tmp/ci-chat-bot/bot.pid` is missing or stale (e.g. the bot was started outside this command), do **not** guess with a broad pattern kill. Instead identify it narrowly and confirm with the user first:
   ```bash
   pgrep -fa ci-chat-bot
   pgrep -fa "make run"
   ```
   Show the full command line(s) to the user. Only proceed to kill if there is exactly one unambiguous match and the user confirms it. If there are multiple matches, stop and ask — never kill on an ambiguous/multi-match result.

2. **Rebuild if needed**: Run `make` to pick up code changes.

3. **Start the bot again** per step 5 above (do **not** touch ngrok), re-recording the new PID to `/tmp/ci-chat-bot/bot.pid`.

4. **Verify**: tail `/tmp/ci-chat-bot/bot.log` to confirm a clean startup, and confirm ngrok is still forwarding (it was never touched).

## Stopping the Test

When the user says to stop testing entirely, stop **both** processes, each verified narrowly before killing (never a broad pattern match):

```bash
if [ -f /tmp/ci-chat-bot/bot.pid ]; then
  PID=$(cat /tmp/ci-chat-bot/bot.pid)
  if ps -p "$PID" -o cmd= | grep -q "ci-chat-bot\|make run"; then
    kill "$PID"
  else
    echo "PID $PID does not look like the ci-chat-bot process; not killing. Inspect manually."
  fi
fi

if [ -f /tmp/ci-chat-bot/ngrok.pid ]; then
  PID=$(cat /tmp/ci-chat-bot/ngrok.pid)
  if ps -p "$PID" -o cmd= | grep -q "ngrok"; then
    kill "$PID"
  else
    echo "PID $PID does not look like the ngrok process; not killing. Inspect manually."
  fi
fi
```

8. **Provide Troubleshooting Tips** if issues arise:
   - Check logs: `tail -100 /tmp/ci-chat-bot/bot.log` to see recent output
   - If ngrok fails to start, verify it's installed (`ngrok version`)
   - If secrets extraction fails, verify cluster access with `oc --context app.ci whoami`
   - If the bot fails to start, check the error messages in the log file
   - Verify that BOT_TOKEN and BOT_SIGNING_SECRET environment variables are set
   - Check that the required external repositories exist:
     - `../release/ci-operator/jobs/openshift/release/` (job configs)
     - `../release/core-services/prow/02_config/_config.yaml` (prow config)
     - `../release/core-services/ci-chat-bot/workflows-config.yaml` (workflow config)
   - The bot runs with `--disable-rosa` flag and verbose logging (`--v=2`) by default
   - If Slack isn't receiving events, verify the ngrok URL is correctly configured in Slack app settings
   - **GCP Credentials dry-run mode**:
     - Check logs for "DRY-RUN mode" message to confirm it's enabled
     - Verify BigQuery audit logs are still being created
     - Confirm IAM policy remains unchanged in GCP Console
     - If testing credentials command, use: `credentials openshift gcp "test message"`
   - **Process management**: the bot and ngrok are tracked via `/tmp/ci-chat-bot/bot.pid` and `/tmp/ci-chat-bot/ngrok.pid`. Always verify a PID's command line with `ps -p "$PID" -o cmd=` before killing it. Never use broad-match kill commands (`pkill -f <generic substring>`, `killall`, `pkill node`/`pkill go`/`pkill ngrok`) — they can match unrelated processes, including the Claude Code CLI's own process.

## Creating an Environment File Template

If the user wants to create an environment file, offer to create a template for them:

```bash
cat > .env.template << 'EOF'
# Required environment variables
BOT_TOKEN=xoxb-your-bot-token-here
BOT_SIGNING_SECRET=your-signing-secret-here

# Optional: GitHub integration
GITHUB_TOKEN=ghp_your-github-token-here

# Optional: GCP Credentials feature
GCP_ACCESS_DRY_RUN=true
GCP_SERVICE_ACCOUNT_JSON={"type":"service_account","project_id":"your-project",...}
ORG_DATA_BUCKET=your-org-data-bucket

# Add any other environment variables your bot needs
EOF
```

Tell the user to:
1. Copy `.env.template` to `.env`
2. Fill in their actual values
3. Never commit `.env` to git (add it to `.gitignore`)
4. Use `.env` when running the bot with the command from step 5

## Testing the Credentials Command

When running in dry-run mode (`GCP_ACCESS_DRY_RUN=true`), you can safely test the credentials command:

1. In Slack, send: `credentials openshift gcp "Testing dry-run mode"`
2. Check logs for: `grep "DRY-RUN" /tmp/ci-chat-bot/bot.log`
3. You should see messages like:
   - `GCP credentials manager running in DRY-RUN mode`
   - `DRY-RUN: Would grant GCP IAM credentials to user...`
4. Verify BigQuery logs (if configured) are still created
5. Confirm no IAM changes were made in GCP Console

Guide the user through the setup process step by step.
