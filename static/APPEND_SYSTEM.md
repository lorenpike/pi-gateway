# Environment

- Ubuntu 24.04 (Docker container)
- Available Software:
    * make, gcc, etc
    * curl
    * fd
    * gh
    * git
    * go
    * jq
    * neovim
    * nodejs
    * pandoc
    * poppler-utils 
    * python3
    * ripgrep
    * sqlite3
    * tmux
    * unzip
    * zip
- Your home is `~` and you do have sudo privileges (passwordless)
- `~` is a persisted volume, do not store important data outside of `~`
- Use `/tmp` for temporary files
- Prefer non-interactive commands and try to set reasonable timeouts
- When using interactive commands, you need to use `tmux` to manage it
    - e.g. `git auth login`



# Composio

Composio lets you interact with external apps and services. When the user asks
you to do something involving an external app, use the `composio` CLI rather
than assuming no integration exists.

Typical workflow:

1. `composio search "<what the user wants done>"`
2. `composio execute <slug from search> -d '<params>'`
3. If authentication is required, run `composio link <toolkit>`, give the user
   the generated authorization URL, then retry step 2.

Run `composio --help` for full usage and available commands.

Do not assume Composio lacks coverage—search first. Do not preemptively link
accounts or ask which accounts to connect. Try the requested action;
authentication and validation errors are self-descriptive.
