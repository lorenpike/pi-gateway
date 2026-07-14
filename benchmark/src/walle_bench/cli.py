from concurrent.futures import Future, ThreadPoolExecutor
import traceback

import click

from .runner import discover

PASS = "o"
FAILURE = "x"
ERROR = "e"
REPORT_WIDTH = 72
Outcome = tuple[str, Exception | None]


@click.command()
@click.option(
    "-j",
    "--jobs",
    default=1,
    type=click.IntRange(min=1),
    show_default=True,
    help="Number of benchmark attempts to run concurrently.",
)
@click.option(
    "-n",
    "--attempts",
    default=3,
    type=click.IntRange(min=1),
    show_default=True,
    help="Number of times to run each benchmark.",
)
@click.option(
    "-k",
    "--keyword",
    metavar="TEXT",
    help="Run tests whose function name contains this text.",
)
@click.option(
    "-v",
    "--verbose",
    is_flag=True,
    help="Print a traceback for every failure and error.",
)
def main(jobs: int, attempts: int, keyword: str | None, verbose: bool) -> None:
    """Discover and run wall-e benchmarks."""

    tests = discover()
    if keyword is not None:
        tests = [test for test in tests if keyword in test.__name__]

    failed = False
    results: list[tuple[str, list[Outcome]]] = []

    with ThreadPoolExecutor(max_workers=jobs) as pool:
        scheduled = [
            (test, [pool.submit(test) for _ in range(attempts)]) for test in tests
        ]

        for test, futures in scheduled:
            outcomes = [_outcome(future) for future in futures]
            results.append((test.__name__, outcomes))
            failed = failed or any(status != PASS for status, _ in outcomes)

    for test_name, outcomes in results:
        _print_result(test_name, outcomes)

    if verbose:
        for test_name, outcomes in results:
            _print_tracebacks(test_name, outcomes)

    if failed:
        raise click.exceptions.Exit(1)


def _outcome(future: Future[None]) -> Outcome:
    try:
        future.result()
    except AssertionError as exc:
        return FAILURE, exc
    except Exception as exc:
        return ERROR, exc
    return PASS, None


def _print_result(test_name: str, outcomes: list[Outcome]) -> None:
    colors = {PASS: "green", FAILURE: "bright_red", ERROR: "red"}
    for status, _ in outcomes:
        click.secho(status, fg=colors[status], nl=False)
    click.echo(f"\t{test_name}")


def _print_tracebacks(test_name: str, outcomes: list[Outcome]) -> None:
    for status, exception in outcomes:
        if status == PASS or exception is None:
            continue
        click.echo(f"{test_name:-^{REPORT_WIDTH}}")
        click.echo("".join(traceback.format_exception(exception)).rstrip())
        click.echo("-" * REPORT_WIDTH)
