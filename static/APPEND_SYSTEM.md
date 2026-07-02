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
