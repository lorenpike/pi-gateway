#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import logging
import secrets
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import webbrowser
from http.server import BaseHTTPRequestHandler, HTTPServer

log = logging.getLogger(__name__)

CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
AUTHORIZE_URL = "https://auth.openai.com/oauth/authorize"
TOKEN_URL = "https://auth.openai.com/oauth/token"
REDIRECT_HOST = "localhost"
REDIRECT_PORT = 1455
REDIRECT_PATH = "/auth/callback"
REDIRECT_URI = f"http://{REDIRECT_HOST}:{REDIRECT_PORT}{REDIRECT_PATH}"
SCOPE = "openid profile email offline_access"
# JWT claim namespace OpenAI puts account info under; matches pi's
# openaiCodexOAuthProvider so the persisted entry is identical to what
# `pi auth login openai-codex` would write.
JWT_CLAIM_PATH = "https://api.openai.com/auth"


def make_pkce() -> tuple[str, str]:
    """Return (code_verifier, code_challenge) using S256."""
    verifier = (
        base64.urlsafe_b64encode(secrets.token_bytes(32)).rstrip(b"=").decode("ascii")
    )
    digest = hashlib.sha256(verifier.encode("ascii")).digest()
    challenge = base64.urlsafe_b64encode(digest).rstrip(b"=").decode("ascii")
    return verifier, challenge


def jwt_payload(token: str) -> dict:
    """Decode (without verifying) the payload of a JWT."""
    try:
        _, payload, _ = token.split(".")
    except ValueError:
        return {}

    # JWT base64url, padding-stripped
    pad = "=" * (-len(payload) % 4)
    data = base64.urlsafe_b64decode(payload + pad)
    try:
        return json.loads(data)
    except json.JSONDecodeError:
        return {}


class CallbackHandler(BaseHTTPRequestHandler):
    """Capture the ?code=... (or ?error=...) from the OAuth redirect."""

    # Populated by the handler so the main thread can read them.
    received_code: dict = {}

    def do_GET(self, _method: str = "GET") -> None:
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path != REDIRECT_PATH:
            self.send_error(404, "Not Found")
            return
        params = urllib.parse.parse_qs(parsed.query)

        if "error" in params:
            CallbackHandler.received_code["error"] = params["error"][0]
            desc = params.get("error_description", [""])[0]
            CallbackHandler.received_code["error_description"] = desc
            body = self._html("Authentication failed", desc, ok=False)
        elif "code" in params:
            CallbackHandler.received_code["code"] = params["code"][0]
            state_ok = (
                CallbackHandler.received_code.get("state")
                == params.get("state", [None])[0]
            )
            body = self._html(
                "Authentication successful",
                "You can close this tab and return to your terminal."
                if state_ok
                else "State mismatch — possible CSRF. Rejecting.",
                ok=state_ok,
            )
        else:
            body = self._html("Waiting...", "No code received.", ok=False)

        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_HEAD = do_GET  # type: ignore[assignment]

    @staticmethod
    def _html(title: str, message: str, *, ok: bool) -> bytes:
        color = "#16a34a" if ok else "#dc2626"
        return (
            "<!doctype html><html><head><meta charset='utf-8'>"
            f"<title>{title}</title></head>"
            f"<body style='font-family:system-ui;text-align:center;padding:3rem'>"
            f"<h1 style='color:{color}'>{title}</h1>"
            f"<p>{message}</p></body></html>"
        ).encode("utf-8")

    def log_message(self, *_args) -> None:  # silence default logging
        pass


def wait_for_callback(state: str, timeout: int = 300) -> str:
    CallbackHandler.received_code = {"state": state}
    server = HTTPServer((REDIRECT_HOST, REDIRECT_PORT), CallbackHandler)
    server.timeout = 1
    deadline = time.time() + timeout
    while time.time() < deadline:
        server.handle_request()
        if "code" in CallbackHandler.received_code:
            server.server_close()
            return CallbackHandler.received_code["code"]
        if "error" in CallbackHandler.received_code:
            server.server_close()
            err = CallbackHandler.received_code["error"]
            desc = CallbackHandler.received_code.get("error_description", "")
            raise RuntimeError(f"OAuth error: {err} — {desc}")
    server.server_close()
    raise TimeoutError("Timed out waiting for the browser callback.")


# --- Flow --------------------------------------------------------------------


def build_auth_url(code_challenge: str, state: str) -> str:
    params = {
        "response_type": "code",
        "client_id": CLIENT_ID,
        "redirect_uri": REDIRECT_URI,
        "scope": SCOPE,
        "code_challenge": code_challenge,
        "code_challenge_method": "S256",
        "state": state,
        "id_token_add_organizations": "true",
        "codex_cli_simplified_flow": "true",
    }
    return f"{AUTHORIZE_URL}?{urllib.parse.urlencode(params)}"


def exchange_code(code: str, code_verifier: str) -> dict:
    """Exchange the authorization code for tokens at the OpenAI token endpoint."""
    body = urllib.parse.urlencode(
        {
            "grant_type": "authorization_code",
            "code": code,
            "redirect_uri": REDIRECT_URI,
            "client_id": CLIENT_ID,
            "code_verifier": code_verifier,
        }
    ).encode("utf-8")
    req = urllib.request.Request(
        TOKEN_URL,
        data=body,
        headers={
            "Accept": "application/json",
            "Content-Type": "application/x-www-form-urlencoded",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:  # noqa: S310
            return json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        text = exc.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"Token exchange failed ({exc.code}): {text}") from exc


def get_account_id(access_token: str) -> str:
    """Extract chatgpt_account_id from the access_token JWT.

    Mirrors pi's ``credentialsFromToken`` logic: the codex responses provider
    sends this as the ``chatgpt-account-id`` header, and pi persists it on the
    credential. We extract it up front so the written auth.json matches what
    ``pi auth login openai-codex`` would produce.
    """
    payload = jwt_payload(access_token) if access_token else {}
    auth = payload.get(JWT_CLAIM_PATH) or {}
    account_id = auth.get("chatgpt_account_id") or ""
    if not isinstance(account_id, str) or not account_id:
        raise RuntimeError(
            "Failed to extract accountId from access token "
            "(no chatgpt_account_id claim under " + JWT_CLAIM_PATH + ")."
        )
    return account_id


def build_output(token_response: dict) -> dict:
    """Build the auth.json entry pi expects for the openai-codex provider.

    pi stores credentials as ``Record<providerId, AuthCredential>`` where an
    OAuth credential is ``{ type: "oauth", access, refresh, expires, ... }``.
    Note ``expires`` is an epoch in **milliseconds** (pi uses
    ``Date.now() + expires_in * 1000``), not seconds.
    """
    access = token_response.get("access_token", "")
    refresh = token_response.get("refresh_token", "")
    id_token = token_response.get("id_token", "")
    expires_in = int(token_response.get("expires_in", 0) or 0)

    # pi uses Date.now() + expires_in * 1000  -> epoch milliseconds.
    expires_ms = int(time.time() * 1000) + expires_in * 1000 if expires_in else 0

    account_id = get_account_id(access)

    # Email isn't required by pi, but OAuthCredentials allows arbitrary extra
    # keys and it's handy to record which account was used.
    claims = jwt_payload(id_token) if id_token else {}
    email = claims.get("email") or claims.get("preferred_email") or ""

    credential: dict = {
        "type": "oauth",
        "access": access,
        "refresh": refresh,
        "expires": expires_ms,
        "accountId": account_id,
    }
    if email:
        credential["email"] = email

    return {"openai-codex": credential}


def write_merged(path: str, entry: dict) -> None:
    """Merge ``entry`` (keyed by provider id) into an existing auth.json, or
    create it. Preserves credentials for any other providers already present.
    """
    existing: dict = {}
    try:
        with open(path, "r", encoding="utf-8") as fh:
            existing = json.load(fh)
        if not isinstance(existing, dict):
            existing = {}
    except FileNotFoundError:
        pass
    except (json.JSONDecodeError, OSError) as exc:
        log.warning("could not read existing %s (%s); overwriting", path, exc)
        existing = {}

    existing.update(entry)
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(existing, fh, indent=2)
        fh.write("\n")


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Codex OAuth login -> pi auth.json (openai-codex credential)."
    )
    parser.add_argument(
        "--no-browser",
        action="store_true",
        help="Don't auto-open the browser; just print the URL.",
    )
    parser.add_argument(
        "--output",
        "-o",
        default=None,
        help="Write/merge the openai-codex credential into this auth.json path "
        "(e.g. .pi/agents/auth.json) instead of printing to stdout. Other "
        "providers already in the file are preserved.",
    )
    parser.add_argument(
        "--log-level",
        default="warning",
        choices=["debug", "info", "warning", "error", "critical"],
        help="Logging verbosity on stderr (default: warning).",
    )
    args = parser.parse_args()

    logging.basicConfig(
        level=getattr(logging, args.log_level.upper()),
        format="%(message)s",
        stream=sys.stderr,
    )

    code_verifier, code_challenge = make_pkce()
    state = secrets.token_urlsafe(16)

    auth_url = build_auth_url(code_challenge, state)
    log.info("Open this URL to sign in: %s", auth_url)
    if not args.no_browser:
        try:
            webbrowser.open(auth_url)
        except Exception:
            log.warning(
                "could not open browser automatically — open the URL above manually"
            )

    log.info(
        "Listening for callback on http://%s:%s%s ...",
        REDIRECT_HOST,
        REDIRECT_PORT,
        REDIRECT_PATH,
    )
    try:
        code = wait_for_callback(state)
    except (TimeoutError, RuntimeError) as exc:
        log.error("%s", exc)
        return 1

    log.debug("Authorization code received; exchanging for tokens...")
    token_response = exchange_code(code, code_verifier)
    entry = build_output(token_response)

    if args.output:
        write_merged(args.output, entry)
        log.warning("Wrote openai-codex credentials to %s", args.output)
    else:
        json.dump(entry, sys.stdout, indent=2)
        sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
