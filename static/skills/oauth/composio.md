# Composio OAuth (`composio`)

Use when the Composio CLI needs authentication.

1. Start the non-blocking headless login flow:

```bash
composio login --no-browser --no-wait --no-skill-install
```

`--no-browser` prevents Composio from trying to run the unavailable `xdg-open`
command. `--no-wait` prints the login details and exits, so this initial command
does not need tmux. `--no-skill-install` prevents the CLI from installing an
unrelated agent skill.

2. Start the cached-login poll in tmux before returning the authorization URL
to the user:

```bash
tmux new-session -d -s composio-login \
  'set -o pipefail; composio login --poll --no-skill-install 2>&1 | tee /tmp/composio-login.log; status=$?; echo "composio login exited $status"; sleep 3600'
tmux list-sessions | grep '^composio-login:'
```

The poll can wait for authorization without blocking the agent. The final sleep
keeps the pane available for inspection after the command exits.

3. Send the user the authorization URL shown in step 1 and ask them to reply
when authorization is complete. The URL contains a temporary `cliKey`, so send
it only to the intended user.

4. After the user confirms authorization, inspect the poll and verify the
account:

```bash
tmux capture-pane -pt composio-login -S -200
composio whoami
```

If the poll is still waiting, ask the user to confirm that authorization
completed. If the pending login has expired, clean up the tmux session, restart
at step 1, and send the new URL.

5. Record the authenticated Composio account in `~/CONTEXT.md` under `# Notes`.
Do not record API keys or the authorization URL.

```text
- Composio is authenticated for ACCOUNT.
```

6. Clean up the tmux session and temporary log:

```bash
tmux kill-session -t composio-login
rm -f /tmp/composio-login.log
```
