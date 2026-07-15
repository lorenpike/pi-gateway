from __future__ import annotations

import os
import secrets
import shutil
import socket
import subprocess
import tempfile
import time
from os import environ
from pathlib import Path

IMAGE = "wall-e"
ENV = (
    "TZ",
    "OPENAI_API_KEY",
    "OPENROUTER_API_KEY",
    "BRAVE_API_KEY",
    "WALLE_PROVIDER",
    "WALLE_MODEL",
)


class Container:
    """Run wall-e in Docker with a lazy temp home mounted at /home/wall-e."""

    def __init__(self):
        self.root: Path | None = None
        self.process: subprocess.Popen | None = None
        self.port: int | None = None
        self.token = secrets.token_hex(32)
        self.log = None

    @property
    def home(self) -> Path:
        if self.root is None:
            self.root = Path(tempfile.mkdtemp(prefix="walle-bench--"))
            (self.root / "home").mkdir()
            seed_home(self.root / "home")
        return self.root / "home"

    @property
    def cidfile(self) -> Path:
        return self.home.parent / "cid"

    @property
    def url(self) -> str:
        if self.port is None:
            raise RuntimeError("container has not been started")
        return f"http://127.0.0.1:{self.port}"

    def start(self) -> None:
        if self.process is not None and self.process.poll() is None:
            return

        if self.cidfile.exists():
            self.cidfile.unlink()

        logs = self.home.parent / "logs"
        logs.mkdir(exist_ok=True)
        self.log = (logs / "docker.log").open("ab")

        self.port = find_free_port()
        # fmt: off
        cmd = [
            "docker", "run", "--rm",
            "--cidfile", str(self.cidfile),
            "-e", f"WALLE_TOKEN={self.token}",
            "-e", f"WALLE_PORT={self.port}",
            "-v", f"{self.home}:/home/wall-e",
            "-p", f"127.0.0.1:{self.port}:{self.port}",
        ]
        # fmt: on
        for key in ENV:
            if key in environ:
                cmd += ["-e", key]
        for source, target in config_mounts():
            cmd += ["-v", f"{source}:{target}:ro"]

        self.process = subprocess.Popen(cmd + [IMAGE], stdout=self.log, stderr=self.log)
        self.cid()

    def execute(self, *command: str, user: str = "wall-e") -> str:
        """Execute a command in the running benchmark container."""
        if self.process is None or self.process.poll() is not None:
            raise RuntimeError("container is not running")
        return subprocess.run(
            ["docker", "exec", "-u", user, self.cid(), *command],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=True,
        ).stdout

    def stop(self) -> None:
        cid = (
            self.cidfile.read_text().strip()
            if self.root and self.cidfile.exists()
            else None
        )
        if cid:
            subprocess.run(
                ["docker", "stop", "--time", "10", cid],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            )

        if self.process is not None and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                if cid:
                    subprocess.run(
                        ["docker", "kill", cid],
                        stdout=subprocess.DEVNULL,
                        stderr=subprocess.DEVNULL,
                        check=False,
                    )
                self.process.kill()
                self.process.wait(timeout=5)

        self.process = None
        self.port = None
        if self.log is not None:
            self.log.close()
            self.log = None

    def clean(self) -> None:
        self.stop()
        if self.root is not None:
            shutil.rmtree(self.root, onerror=remove_readonly)
            self.root = None

    def cid(self, timeout: float = 10) -> str:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.cidfile.exists() and (cid := self.cidfile.read_text().strip()):
                return cid
            if self.process is not None and self.process.poll() is not None:
                raise RuntimeError("container exited before Docker wrote cidfile")
            time.sleep(0.1)
        raise RuntimeError("timed out waiting for Docker cidfile")

    def __del__(self):
        try:
            self.stop()
        except Exception:
            pass


def find_free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def seed_home(home: Path) -> None:
    cid = subprocess.run(
        ["docker", "create", IMAGE],
        text=True,
        stdout=subprocess.PIPE,
        check=True,
    ).stdout.strip()
    try:
        subprocess.run(
            ["docker", "cp", f"{cid}:/home/wall-e/.", str(home)],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=True,
        )
        fix_home_permissions(home)
    finally:
        subprocess.run(
            ["docker", "rm", "-f", cid],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )


def fix_home_permissions(home: Path) -> None:
    subprocess.run(
        [
            "docker",
            "run",
            "--rm",
            "-v",
            f"{home}:/home/wall-e",
            IMAGE,
            "sh",
            "-c",
            "chown -R wall-e:wall-e /home/wall-e && chmod -R u+rwX /home/wall-e",
        ],
        stdout=subprocess.DEVNULL,
        check=True,
    )


def remove_readonly(func, path, _exc_info) -> None:
    os.chmod(path, 0o700)
    func(path)


def config_mounts() -> list[tuple[Path, str]]:
    root = Path(__file__).resolve().parents[3]
    candidates = [
        (root / "build" / "auth.json", "/opt/pi/auth.json"),
        (root / "build" / "pi-settings.json", "/opt/pi/settings.json"),
    ]
    return [(source, target) for source, target in candidates if source.exists()]
