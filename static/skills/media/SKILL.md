---
name: media
description: Send text, images, audio, and other files back to the current wall-e channel.
---

# Media

Use this skill when you need to send a file or direct text message back to the
user's wall-e channel without starting a new agent prompt.

## Current channel

The current wall-e channel is available while you are inside a live turn:

```sh
printf '%s\n' "$WALLE_CHANNEL"
```

It has the form `<type>:<id>`, for example:

```text
telegram:123456789
http:morning-digest
```

Use this value unless the user explicitly asks to send somewhere else.

## Send text

Send a direct text message:

```sh
wall-e send "$WALLE_CHANNEL" "Your message here"
```

For multi-line text, pipe stdin:

```sh
wall-e send "$WALLE_CHANNEL" <<'EOF'
Long message here.
EOF
```

## Send a file

Use `--media` with an absolute file path:

```sh
wall-e send --media "$WALLE_CHANNEL" /home/wall-e/path/to/file.png
```

With a caption:

```sh
wall-e send --media "$WALLE_CHANNEL" /home/wall-e/path/to/report.pdf --caption "Here is the report."
```

Prefer files under `/home/wall-e` or another path accessible to the wall-e
process. Check that the file exists before sending.

## Check result

`wall-e send` writes machine-readable status to stdout. Check the exit code and
stdout. On success it returns JSON like:

```json
{"ok":true,"channel":"telegram:123456789"}
```

On failure it exits non-zero and returns JSON like:

```json
{"ok":false,"error":"telegram: file too large"}
```

If sending fails, tell the user what failed and offer an alternative, such as a
smaller file, a different format, or a text summary.

## Notes

- Do not assume every channel supports every media type.
- Let `wall-e send` report channel-specific failures.
- Use absolute paths for files.
- Do not paste base64 media into chat unless explicitly requested.
