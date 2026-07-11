from __future__ import annotations

import json
import secrets
import time
from pathlib import Path

import requests

from .docker import Container


class Agent:
    """Benchmark target backed by a wall-e Docker container."""

    def __init__(self):
        self.container = Container()
        self.channel = f"bench-{secrets.token_hex(6)}"
        self.messages: list[tuple[str, str]] = []

    @property
    def workspace(self) -> Path:
        return self.container.home

    @property
    def transcript(self) -> list[dict[str, str]] | None:
        try:
            transcript_file = next(
                (self.workspace / "sessions").glob(f"*{self.channel}*.jsonl")
            )
        except StopIteration:
            return None

        with open(transcript_file, "r", encoding="utf-8") as f:
            return [json.loads(line) for line in f]

    def __enter__(self) -> Agent:
        self.start()
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.stop()

    def __call__(self, message: str | None) -> str:
        if message is None:
            raise ValueError("Agent requires a prompt")
        if self.container.process is None or self.container.process.poll() is not None:
            raise RuntimeError("Agent is not running; use `with Agent() as agent:`")

        self.messages.append(("user", message))

        response = requests.post(
            f"{self.container.url}/v1/prompt",
            headers={"Authorization": f"Bearer {self.container.token}"},
            json={"channel": self.channel, "message": message},
            stream=True,
            timeout=300,
        )
        response.raise_for_status()

        reply = read_sse_response(response)
        self.messages.append(("agent", reply))
        return reply

    def new_session(self) -> None:
        self.messages.clear()
        self.channel = f"bench-{secrets.token_hex(6)}"

    def start(self) -> None:
        self.container.start()
        self.wait_ready()

    def stop(self) -> None:
        self.container.stop()

    def clean(self) -> None:
        self.container.clean()

    def wait_ready(self, timeout: float = 30) -> None:
        deadline = time.monotonic() + timeout
        last_error = None

        while time.monotonic() < deadline:
            try:
                response = requests.get(f"{self.container.url}/health", timeout=1)
                if response.ok:
                    return
            except requests.RequestException as exc:
                last_error = exc

            time.sleep(0.2)

        raise RuntimeError(f"agent container did not become ready: {last_error}")

    def __del__(self):
        try:
            self.stop()
        except Exception:
            pass


def read_sse_response(response: requests.Response) -> str:
    text = []
    event = None
    data = []

    def dispatch() -> bool:
        nonlocal event, data
        if event is None:
            return False

        payload = "\n".join(data)
        if event == "delta":
            text.append(json.loads(payload).get("text", ""))
        elif event == "error":
            message = json.loads(payload).get("message", payload)
            raise RuntimeError(f"agent error: {message}")
        elif event == "done":
            return True

        event = None
        data = []
        return False

    for line in response.iter_lines(decode_unicode=True):
        if line == "":
            if dispatch():
                break
        elif line.startswith("event: "):
            event = line.removeprefix("event: ")
        elif line.startswith("data: "):
            data.append(line.removeprefix("data: "))

    return "".join(text)
