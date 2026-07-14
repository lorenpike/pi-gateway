# Composio OAuth (`composio`)

Use when the Composio CLI needs authentication.

1. Start login in tmux so it stays alive without blocking the agent:

```bash
tmux new-session -d -s composio-oauth 'composio login --no-browser; status=$?; echo; echo "composio login exited $status"; sleep 3600'
tmux capture-pane -pt composio-oauth -S -200
```

`--no-browser` is required in the headless container; otherwise Composio tries
to run the unavailable `xdg-open` command.

2. Send the user the authorization URL shown by Composio and ask them to reply
when authorization is complete. The URL contains a temporary `cliKey`, so send
it only to the intended user.

3. Check the tmux session for completion:

```bash
tmux capture-pane -pt composio-oauth -S -200
```

4. Confirm login:

```bash
composio whoami
```

5. Record the authenticated Composio account in `~/CONTEXT.md` under `# Notes`.
Do not record API keys or the authorization URL.

```text
- Composio is authenticated for ACCOUNT.
```

6. Clean up tmux:

```bash
tmux kill-session -t composio-oauth
```
