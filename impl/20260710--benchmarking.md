# Benchmarking schema — Planning

**Date:** 2026-07-10

## Goal

Define a benchmark format and Python runner for wall-e agent behavior. A
benchmarking unit is a **test**: a probabilistic interaction with the agent and
its environment, followed by deterministic, LLM, and/or manual evaluation of the
resulting transcript and artifacts.

The design should optimize for:

- repeated attempts because both the agent and graders are flaky;
- parallel execution because each attempt can take minutes;
- inspectable failure diagnosis with complete transcripts and persisted volumes;
- scriptable Python tests that are still readable as benchmark specs;
- support for helper agents whose outputs become inputs to the target agent.

## Vocabulary

- **Test**: named benchmark spec. Defines setup, prompt/action sequence,
  timeout, attempts, expected artifacts, and grading policy.
- **Attempt**: one execution of a test. Attempts are isolated and produce their
  own transcript, result metadata, and filesystem snapshot.
- **Suite**: a collection of tests selected by file, tag, name, or directory.
- **Setup**: preparation of the target environment and starting conversation.
  Setup may seed files, environment variables, volumes, prior messages, helper
  services, or helper agents.
- **Prompt**: the trigger sent to the agent under test. A test may have one
  prompt or a scripted sequence of prompts.
- **Result**: all outputs from an attempt: final agent response, SSE/RPC events,
  exported session transcript, files, generated artifacts, logs, and captured
  container volume.
- **Grader**: evaluator of the result. Graders may be deterministic Python
  checks, LLM checks, or manual review items. An LLM grader may be injected into
  a copy of the environment to inspect artifacts.
- **Target**: the agent plus its runtime environment under test: model, pi
  settings, wall-e gateway, container image, system prompt, mounted home volume,
  and any configured tools.
- **Run**: one invocation of the runner over one or more tests and attempts.

## Test lifecycle

```text
select tests
  -> create isolated attempt workspace
  -> setup target environment
  -> run prompt/action sequence with timeout
  -> collect response, transcript, logs, artifacts, final volume
  -> run graders
  -> summarize pass/fail/flake data
  -> keep or clean attempt workspace based on policy
```

Every attempt should have stable IDs:

```text
run_id      20260710T193012Z-a83f
attempt_id 20260710T193012Z-a83f/onboarding_identity/003
channel    bench-onboarding-identity-003-a83f
```

Use the attempt ID in channels, temp paths, log filenames, and result JSON so
parallel attempts do not collide. In Docker mode the harness should avoid fixed
container names; use Docker's generated name plus a `--cidfile` when the runner
needs to stop the container.

## Python package and API shape

Use a real Python package rather than a single script:

```text
benchmark/
  pyproject.toml                  setuptools build system
  src/walle_bench/
    client.py                     simulated human/helper LLM client
    docker.py                     low-level Docker lifecycle helper
    agent.py                      target agent wrapper around the container API
    utils.py                      small benchmark utilities such as timeout
  tests/                          current exploratory benchmark-style tests
```

`walle_bench` already names the domain, so avoid redundant class names such as
`WallEContainer`. Prefer simple names at the module boundary:

```python
from walle_bench import Agent, Client, timeout
from walle_bench.docker import Container
```

Current benchmark case style:

```python
from itertools import cycle, islice

from walle_bench import Agent, Client, timeout


def test_onboarding():
    client = Client("""
    Your name is Matt. You are a personal tax accountant at Acme Accounting.
    Say hi to kick off onboarding. When done, say 'hotcakes'.
    """)

    with Agent() as agent, timeout(300):
        response = None
        for bot in islice(cycle([client, agent]), 25):
            response = bot(response)
            if bot is client and "hotcakes" in response:
                break

        context_md = (agent.workspace / "CONTEXT.md").read_text()
        assert "Matt" in context_md
        assert "Acme Accounting" in context_md
```

Longer-term declarative `Test(...)` objects can still be layered on top of this,
but the immediate framework should preserve the readability of plain Python.

## Isolation, home seeding, and parallelism

Canonical benchmark runs should use **one fresh container per attempt**. Reusing
containers can hide bugs or create false failures because the agent mutates its
home directory, session state, context, caches, and generated artifacts.

The current Docker strategy is:

- Build/reuse a single `wall-e` image for the run.
- For each attempt, create a temporary host root with a lazy `home/` directory.
- Seed `home/` by copying `/home/wall-e` from a temporary image container.
- Normalize the seeded tree with `chown -R wall-e:wall-e` and
  `chmod -R u+rwX` so the running container can mutate it.
- Bind mount that host directory as `/home/wall-e`.
- Run Docker with `--rm`, no explicit container name, and a `--cidfile` so the
  harness can stop the container.
- Use `socket` to find a free host/container `WALLE_PORT` per attempt. Do not
  hard-code a benchmark port; parallel runs must not collide.
- Use a unique `WALLE_TOKEN` and channel per attempt.
- Stop the container at timeout/completion. Keep the temp root for inspection by
  default; call `clean()` when the benchmark should delete it.

The low-level `Container` API should stay small:

```python
container = Container()
container.home       # lazy seeded host Path mounted to /home/wall-e
container.start()    # starts docker, assigns container.port, writes logs
container.url        # http://127.0.0.1:<port>
container.process    # attached docker run process
container.stop()     # stop container, keep home for inspection
container.clean()    # stop and remove the temp root
```

`Agent` is the higher-level context manager:

```python
with Agent() as agent:
    reply = agent("hi")
    context = (agent.workspace / "CONTEXT.md").read_text()
# __exit__ stops the container; clean() remains explicit for inspection control
```

An existing shared HTTP target can still be useful later for quick development,
but it should be marked non-canonical because it lacks filesystem isolation.

## Runner CLI

Proposed package command once the runner graduates from exploratory tests:

```sh
# list discovered cases
uv run --project benchmark walle-bench list benchmark/cases/

# run one case once while developing
uv run --project benchmark walle-bench run benchmark/cases/onboarding.py::onboarding_identity --attempts 1 --keep-all

# run a suite in parallel
uv run --project benchmark walle-bench run benchmark/cases/ --jobs 4 --attempts 5

# inspect a previous run
uv run --project benchmark walle-bench show build/bench/runs/20260710T193012Z-a83f

# rerun failed tests from a previous run
uv run --project benchmark walle-bench rerun-failed build/bench/runs/20260710T193012Z-a83f
```

Useful flags:

- `--jobs N`: parallel attempts.
- `--attempts N`: override test default attempts.
- `--timeout SECONDS`: override test default timeout.
- `--target docker|existing-http`.
- `--keep-all`, `--keep-failed`, `--clean`.
- `--model`, `--provider`: target model overrides.
- `--grader-model`, `--grader-provider`: grader overrides.
- `--tag TAG`, `--name PATTERN`: selection.
- `--json`: machine-readable summary to stdout.

## Result layout

Write all benchmark output under `build/bench/runs/<run_id>/`.

```text
build/bench/runs/<run_id>/
  run.json                         run metadata, git sha, image digest, CLI args
  summary.md                       human-readable report
  summary.json                     per-test pass rates and failure reasons
  tests/<test_name>/attempt-001/
    attempt.json                   timings, target config, grades, status
    prompt.txt                     rendered prompt sent to target
    response.txt                   final agent response text
    events.jsonl                   raw SSE/RPC event stream
    transcript.html                exported wall-e/pi transcript when available
    transcript.jsonl               normalized transcript when available
    home/                          bind-mounted /home/wall-e; final persisted state
    logs/
      docker.log                   attached docker stdout/stderr
      gateway.log                  optional extracted app log
      supervisor.log               optional extracted supervisor log
    artifacts/
      CONTEXT.md                   optional copied convenience artifact
      generated-files/...
    graders/
      deterministic.json
      llm-context_identity_rubric.prompt.txt
      llm-context_identity_rubric.response.json
      manual.md
```

The bind-mounted `home/` is the primary volume snapshot. It should normally be
kept after failures and during local development because it is the easiest way to
inspect `CONTEXT.md`, sessions, generated files, and any other persisted agent
state. Failure diagnosis also requires `events.jsonl`, transcript export, and
container logs even if the attempt times out. If the runner cannot collect one of
these, it should record that as a collection error in `attempt.json` rather than
failing silently.

## Grading model

Each grader returns structured data:

```json
{
  "name": "context_identity_rubric",
  "kind": "llm",
  "pass": true,
  "score": 0.92,
  "reason": "CONTEXT.md records Carl Van Boom, Ardian Financial Consulting, Winnipeg, and Isaac.",
  "evidence": ["artifacts/CONTEXT.md"]
}
```

Grader types:

- **Deterministic**: file exists, text contains, regex match, JSON path, command
  exit code, image/PDF existence, HTTP response, etc.
- **LLM out-of-band**: grader receives transcript/artifacts as input and returns
  strict JSON.
- **LLM injected**: runner starts a grader agent against a copied/read-only
  attempt volume and asks it to inspect files. This is useful for rich artifacts
  and for evaluating what another agent would observe inside the environment.
- **Manual**: written checklist items that block final acceptance but do not
  block automated CI unless `--require-manual` is set.

Because graders are also probabilistic, LLM graders should support
`grader_attempts` and a decision policy such as majority vote or average score.
The result must record every grader response, not only the aggregate.

## Pass/fail and flake policy

A test should define both attempt-level and test-level success:

```python
PassPolicy(
    required_graders="all",      # all | any | named set
    min_attempt_passes=3,        # e.g. 3 of 5
    max_timeout_rate=0.2,
    min_mean_score=0.8,
)
```

Report at least:

- pass count / attempts;
- timeout count;
- deterministic failure count;
- LLM grader disagreement count;
- mean and median runtime;
- links to failed attempt directories.

## Agent-to-agent onboarding example

The gist example uses two lightweight agents and pipes one response into the
other. For wall-e, encode that as a helper-agent action followed by a target
prompt action.

Test intent:

1. A helper agent produces Carl's onboarding opener from this instruction:

   > You're name is Carl Van Boom. You work for Ardian Financial Consulting a
   > company from Winnepeg. You are setting up a computer agent which you will
   > name Isaac. Please start with "Hi" to begin the agent onboarding process.

2. The helper's output is sent to the wall-e target.
3. The target should onboard the user and persist identity facts.
4. The runner collects the persisted volume and `CONTEXT.md`.
5. Checks verify that `CONTEXT.md` contains:
   - user name: Carl Van Boom;
   - company: Ardian Financial Consulting;
   - location: Winnipeg/Winnepeg, with typo-tolerant grading if desired;
   - desired agent name: Isaac.

Open question: this may require a multi-turn action sequence if the target asks
clarifying questions during onboarding. The schema should allow `SendPrompt` to
feed the target's response back into the helper agent for N turns, similar to the
linked gist:

```python
ConversationLoop(
    speakers=[Helper("carl"), TargetAgent()],
    initial_ref="carl_opening",
    max_turns=6,
    stop_when=LLMStopCondition("The target has completed onboarding."),
)
```

## Implementation milestones

1. Create the `benchmark/` Python package with setuptools/uv metadata. **Done.**
2. Add a simple OpenAI-backed `Client` for simulated humans/helper agents.
   **Done.**
3. Add a lazy Docker `Container` helper with temp home seeding, dynamic port
   selection, `start()`, `stop()`, `clean()`, `home`, and `process`. **Done.**
4. Add an `Agent` context manager that starts/stops the container and sends
   prompts to `/v1/prompt` while parsing SSE deltas. **Done.**
5. Improve event capture so every prompt stores raw SSE to `events.jsonl`, not
   just the final assembled response.
6. Add result directory management under `build/bench/runs/<run_id>/`.
7. Add deterministic graders and a first summary report.
8. Add transcript/session export collection.
9. Add LLM graders with strict JSON responses and stored prompts/responses.
10. Add attempts/jobs runner with per-attempt containers and timeout cleanup.
11. Move exploratory benchmark-style tests into a clearer `benchmark/cases/`
    directory once the runner exists.
12. Add `pyinstrument` profiling command/docs for slow benchmark diagnosis.

## Open questions

- Should CI run a small smoke subset, or are all benchmarks manual/on-demand due
  to model cost and credentials?
- Should exploratory benchmark cases remain under `benchmark/tests/` briefly, or
  move immediately to `benchmark/cases/` to avoid pytest/unit-test confusion?
- Should `Agent.__exit__` only `stop()` (current preference, keeps home for
  inspection) or optionally `clean()` under a runner-controlled policy?
- Should the injected grader run with write access, or only against a read-only
  copy of the attempt home? Default should be read-only/copy-on-write.
- How much model/provider metadata can pi expose in transcripts for later
  reproducibility?
