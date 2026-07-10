from dataclasses import dataclass, field
from os import environ
from textwrap import dedent
from typing import Any

import requests

OPENAI_API_KEY = environ["OPENAI_API_KEY"]


@dataclass
class Client:
    """A simple llm client to simulate a human user"""

    initial_message: str
    model: str = "gpt-5.4-mini"
    timeout: float = 20  # seconds
    messages: list[dict[str, str]] = field(default_factory=list)

    def __post_init__(self) -> None:
        self.messages.append(
            {"role": "user", "content": dedent(self.initial_message).strip()}
        )

    def __call__(self, message: str | None) -> str:
        if message is not None:
            self.messages.append({"role": "user", "content": message})

        response = requests.post(
            "https://api.openai.com/v1/responses",
            headers={
                "Authorization": f"Bearer {OPENAI_API_KEY}",
                "Content-Type": "application/json",
            },
            json={
                "model": self.model,
                "input": [
                    {"role": "developer", "content": self.dev_prompt},
                    *self.messages,
                ],
            },
            timeout=self.timeout,
        )

        response.raise_for_status()

        reply = extract_text(response.json())
        if not reply:
            raise RuntimeError("Client model response did not contain output text")

        self.messages.append({"role": "assistant", "content": reply})
        return reply

    @property
    def dev_prompt(self) -> str:
        return """
        You are part of a benchmarking suite to evaluate chatbots. The first
        message will contain instructions on how to interact. Following
        messages will be from the bot. Act like a human.
        """.strip()


def extract_text(data: dict[str, Any]) -> str:
    """Extract text from an OpenAI Responses API response."""

    if isinstance(data.get("output_text"), str):
        return data["output_text"].strip()

    chunks: list[str] = []
    for item in data.get("output", []):
        if item.get("type") != "message":
            continue

        for content in item.get("content", []):
            if content.get("type") in {"output_text", "text"}:
                text = content.get("text")
                if isinstance(text, str):
                    chunks.append(text)

    return "".join(chunks).strip()
