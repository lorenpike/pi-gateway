from __future__ import annotations

import inspect
import runpy
from collections.abc import Callable
from pathlib import Path
from typing import Any


def discover() -> list[Callable[..., Any]]:
    """Load benchmark files and return their ``test_*`` functions."""
    try:
        tests_dir = next(Path.cwd().glob("tests"))
    except StopIteration:
        raise RuntimeError("No test folder")

    tests = []
    for index, test_file in enumerate(sorted(tests_dir.rglob("test_*.py"))):
        module_name = f"_walle_bench_test_{index}_{test_file.stem}"
        namespace = runpy.run_path(str(test_file), run_name=module_name)

        tests.extend(
            value
            for name, value in namespace.items()
            if name.startswith("test_")
            and inspect.isfunction(value)
            and value.__module__ == module_name
        )
    return tests
