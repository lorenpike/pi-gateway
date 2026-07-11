from importlib import resources

from .agent import Agent
from .client import Client
from .utils import timeout

static = resources.files("walle_bench.static")

__all__ = ["Agent", "Client", "timeout", "static"]
