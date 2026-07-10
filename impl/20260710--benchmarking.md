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

Use the attempt ID in channels, temp paths, Docker names, volume names, log
filenames, and result JSON so parallel attempts do not collide.

## Schema shape

Prefer Python-first specs, with dataclasses/Pydantic-style models in the runner.
YAML can be added later as a thin serialization layer, but Python makes scripted
setup, helper agents, and custom checks easier to read and maintain.

Sketch:

```python
Test(
    name="onboarding_identity",
    tags={"onboarding", "context"},
    attempts=5,
    timeout_s=600,
    target=Target(
        mode="docker",              # docker | existing_http
        image="wall-e:bench",       # or build from current tree
        pool_size=1,
        model="...",                # optional override
        provider="...",             # optional override
        keep_volume="on_fail",      # never | on_fail | always
    ),
    setup=[
        SeedFile("/home/wall-e/CONTEXT.md", ""),
        HelperAgent(
            name="carl",
            provider="openai",
            model="gpt-4o",
            system="You are Carl Van Boom...",
        ),
    ],
    actions=[
        HelperPrompt(
            helper="carl",
            prompt="""
            You're name is Carl Van Boom. You work for Ardian Financial
            Consulting a company from Winnepeg. You are setting up a computer
            agent which you will name Isaac. Please start with "Hi" to begin
            the agent onboarding process.
            """,
            save_as="carl_opening",
        ),
        SendPrompt(input_ref="carl_opening"),
    ],
    collect=[
        CollectSession(),
        CollectPath("/home/wall-e/CONTEXT.md"),
        CollectVolumeSnapshot(),
    ],
    graders=[
        ContainsText(path="/home/wall-e/CONTEXT.md", text="Carl Van Boom"),
        ContainsText(path="/home/wall-e/CONTEXT.md", text="Ardian Financial Consulting"),
        LLMGrade(
            name="context_identity_rubric",
            inject_environment=True,
            prompt="""
            Inspect the final CONTEXT.md and transcript. Return JSON with
            pass=true only if the agent has correctly recorded the user's name,
            company, location, and desired agent name.
            """,
        ),
        ManualCheck("Confirm CONTEXT.md has the right user/company and no obvious hallucinated identity."),
    ],
    pass_policy=PassPolicy(min_attempt_passes=3),
)
```

## Isolation and parallelism

Two target modes are useful:

1. **Hermetic Docker mode** for real benchmarks.
   - Build or reuse the wall-e image.
   - Start one container per attempt or one container per worker.
   - Mount a unique home/session volume per attempt.
   - Use a unique `WALLE_TOKEN`, `WALLE_PORT`, and channel.
   - Stop the container at timeout or completion.
   - Preserve the volume snapshot on failure or when requested.

2. **Existing HTTP mode** for quick development.
   - Runner points at `WALLE_URL` and `WALLE_TOKEN`.
   - Each attempt uses a unique channel.
   - Faster, but not fully isolated because the backing container and persisted
     home may be shared.

Parallelism should be controlled at the runner level:

```sh
uv run scripts/bench.py run benchmarks --jobs 4 --attempts 5 --timeout 600
```

The default `--jobs` should not exceed the wall-e pool size unless running one
container per attempt. Timeouts must terminate the prompt stream and then collect
whatever logs/artifacts are available.

## Runner CLI

Proposed commands:

```sh
# list discovered tests
uv run scripts/bench.py list benchmarks/

# run one test once while developing
uv run scripts/bench.py run benchmarks/onboarding.py::onboarding_identity --attempts 1 --keep-all

# run a suite in parallel
uv run scripts/bench.py run benchmarks/ --jobs 4 --attempts 5

# inspect a previous run
uv run scripts/bench.py show build/bench/runs/20260710T193012Z-a83f

# rerun failed tests from a previous run
uv run scripts/bench.py rerun-failed build/bench/runs/20260710T193012Z-a83f
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
    logs/
      container.log
      gateway.log
      supervisor.log
    artifacts/
      CONTEXT.md
      generated-files/...
    volume/                        optional copied persisted volume
    graders/
      deterministic.json
      llm-context_identity_rubric.prompt.txt
      llm-context_identity_rubric.response.json
      manual.md
```

Failure diagnosis requires `events.jsonl`, transcript export, container logs, and
collected files even if the attempt times out. If the runner cannot collect one
of these, it should record that as a collection error in `attempt.json` rather
than failing silently.

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

1. Add `scripts/bench.py` with a tiny Python model, test discovery, and `list`.
2. Implement existing-HTTP target mode and SSE capture for `/v1/prompt`.
3. Add deterministic graders and result directory layout.
4. Add Docker target mode with isolated volumes and timeout cleanup.
5. Add transcript/session export collection.
6. Add LLM graders with strict JSON responses and stored prompts/responses.
7. Add helper agents and conversation loops.
8. Add summary markdown/json reporting and rerun-failed support.
9. Promote one or two onboarding/context tests into `benchmarks/`.

## Open questions

- Should benchmarks live in `benchmarks/`, `tests/benchmarks/`, or `impl/benchmarks/`?
- Should CI run a small smoke subset, or are all benchmarks manual/on-demand due
  to model cost and credentials?
- What is the canonical target volume path to snapshot in Docker mode:
  `/home/wall-e`, `/home/wall-e/sessions`, or the full container diff?
- Should the injected grader run with write access, or only against a read-only
  copy of the attempt volume? Default should be read-only/copy-on-write.
- How much model/provider metadata can pi expose in transcripts for later
  reproducibility?
