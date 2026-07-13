# TODO

- [x] Fix slow and misleading `wall-e send --media` failures.
  - The image path already used `sendPhoto`, but any photo error—including an ambiguous transport timeout—was silently discarded before retrying with `sendDocument`. Two sequential timeouts could therefore take 30–180 seconds and report only the misleading document error.
  - Telegram polling also mutated the timeout on the shared `http.Client` while sends used it concurrently, and the persistent HTTP/2 connection had no health check. During the reported incident, gateway logs showed `getUpdates` and `sendChatAction` timing out too, confirming this was not specific to PNG dimensions.
  - Fixed by using a context deadline for long polling without mutating the client, health-checking persistent HTTP/2 connections, falling back to `sendDocument` only after a definitive Telegram rejection, and preserving the original photo error when fallback fails.
  - Transport errors now redact the token-bearing Telegram URL before reaching logs or `/v1/send` JSON.

- [ ] Install skill npm dependencies at Docker build time.
  - Some skills require `npm install` before they can run.
  - Detect skills with `package.json` and run install during the Docker image build so runtime startup/use does not pay this cost.

- [ ] Fix markdown-to-HTML parsing; see Appendix A.

- [ ] Manage additional message modalities.
  - Support images.
  - Support speech/audio.

## Appendix A: Markdown formatting regressions

Add improperly formatted message examples below in `<?--example-->` blocks so they can be turned into parser tests.

<?--example-->
With the current setup, email could be handled as another **front-end/channel adapter**, similar to Telegram or HTTP.

Current architecture already has the useful core pieces:

```text
Email provider / polling job
        ↓
email adapter
        ↓
turn.Manager.Submit(channel="email:<thread-or-address>", message)
        ↓
pi worker pool
        ↓
assistant events
        ↓
email adapter sends reply email
```

Practical options:

1. **Quick/simple: cron + `wall-e msg`**
   - A cron job checks an inbox via IMAP/Gmail API/Microsoft Graph.
   - For each new email, format a prompt and submit:

   ```sh
   wall-e msg email:thread-abc123 <<'EOF'
   From: person@example.com
   Subject: Question

   Email body here...
   EOF
   ```

   This would get the assistant response on stdout. The script could then send it via SMTP/API.

2. **Better: native email adapter inside `wall-e`**
   - Add `channelType: "email"`.
   - Use a stable channel ID like:
     - `email:<thread-id>`
     - `email:<message-id>`
     - `email:<sender-address>`
   - Adapter receives inbound email, submits to the turn manager, subscribes to assistant deltas, then sends a reply email when complete.

3. **Webhook-based email**
   - Use Mailgun, SendGrid, Postmark, AWS SES, etc.
   - Provider POSTs inbound email to a new endpoint like:

   ```text
   POST /v1/email/inbound
   ```

   - Gateway parses sender/body/thread headers and routes to `turn.Manager`.

4. **Polling-based email**
   - If avoiding public webhooks, run a small daemon or cron job inside/near the container that polls IMAP.
   - This is easier to deploy locally, but less real-time and more stateful.

Important design choice: **channel identity**.

For email, I’d probably use thread-level channels:

```text
email:<provider-thread-id>
```

That means an email conversation reuses the same pi session/history. If no thread ID exists, fallback to:

```text
email:<normalized-subject-or-message-id>
```

Main missing pieces:

- Inbound email parser.
- Outbound email sender.
- Mapping provider thread/message IDs to gateway channel IDs.
- Attachment handling, probably overlaps with the “additional modalities: images/audio” TODO.
- Safety controls: allowed senders, max body size, maybe approval before sending external replies.

So: email is very compatible with the current channel architecture. The fastest version is a script using `wall-e msg`; the clean version is a first-class `email` adapter.
<?--example-->
