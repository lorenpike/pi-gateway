from importlib import resources
from pathlib import Path

from .agent import Agent
from .client import Client
from .utils import timeout

static = resources.files("walle_bench.static")


def seed(src: str, dst: Path) -> None:
    """Copy a file from the static folder to a target path."""
    source = static.joinpath(src)
    dst.parent.mkdir(parents=True, exist_ok=True)
    dst.write_bytes(source.read_bytes())


__all__ = ["Agent", "Client", "timeout", "seed", "static"]
