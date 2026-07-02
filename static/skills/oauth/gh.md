# GitHub OAuth (`gh`)

Use when `gh` needs GitHub authentication.

1. Start the device login in tmux so it stays alive:

```bash
tmux new-session -d -s gh-oauth 'gh auth login --hostname github.com --git-protocol https --web; echo; echo "gh auth login exited $?"; sleep 3600'
tmux capture-pane -pt gh-oauth -S -200
```

2. Send the user the device URL and code shown by `gh`.

Usually:

```text
Open https://github.com/login/device and enter code: XXXX-XXXX
Reply here when GitHub says authentication is complete.
```

3. Let `gh` continue if it is waiting for Enter:

```bash
tmux send-keys -t gh-oauth Enter
```

4. Confirm login:

```bash
gh auth status --hostname github.com
gh api user --jq .login
```

5. Record it in `~/CONTEXT.md` under `# Notes`:

```text
- gh is authenticated for user @USERNAME.
```

6. Clean up tmux:

```bash
tmux kill-session -t gh-oauth
```
