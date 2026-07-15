from datetime import date, datetime, time, timedelta
import re
from textwrap import dedent

from walle_bench import Agent, seed, timeout

TIMEOUT = 120  # seconds


def test_sets_up_rio_weather_cron():
    prompt = dedent("""\
        Set up a log that records the current weather in Rio de Janeiro at the
        top of every hour. Use wttr.in to get the weather.
        """)

    with timeout(TIMEOUT), Agent(clean=True) as agent:
        seed("contexts/minimal.md", agent.workspace / "CONTEXT.md")
        _ = agent(prompt)

        agent.new_session()
        response = agent("What are the current cron jobs and what do they do?")

        response = response.lower()
        assert "rio" in response, f"Rio missing from response: {response}"
        assert "weather" in response, f"weather missing from response: {response}"
        assert "hour" in response, f"hourly schedule missing from response: {response}"


def test_set_reminder_to_mail_mom():
    prompt = dedent("""\
        Set a reminder to mail my mom a birthday card at 4:45pm tomorrow.
        """)

    with Agent(clean=True) as agent:
        seed("contexts/minimal.md", agent.workspace / "CONTEXT.md")
        _ = agent(prompt)

        job_id, scheduled = _single_at_job(agent)
        assert scheduled.date() == _container_today(agent) + timedelta(days=1), (
            f"reminder scheduled for {scheduled.date()}, want tomorrow"
        )
        assert scheduled.time() == time(16, 45), (
            f"reminder scheduled for {scheduled.time()}, want 16:45"
        )

        payload = agent.container.execute("at", "-c", job_id).lower()
        scripts = "\n".join(
            path.read_text(encoding="utf-8", errors="replace").lower()
            for path in agent.workspace.rglob("*.sh")
        )
        intent = payload + "\n" + scripts
        missing = [word for word in ("mom", "birthday", "card") if word not in intent]
        assert not missing, f"scheduled reminder missing intent terms: {missing}"
        assert any(action in intent for action in ("mail", "send", "post")), (
            "scheduled reminder does not say to mail/send/post the card"
        )

        agent.new_session()
        response = agent("What jobs are currently scheduled with `at`?").lower()

        assert any(person in response for person in ("mom", "mother")), (
            f"mom job missing from response: {response}"
        )
        assert "birthday" in response and "card" in response, (
            f"birthday-card reminder missing from response: {response}"
        )
        assert re.search(r"(?:\b4:45\s*(?:p\.?m\.?)?|\b16:45)", response), (
            f"4:45pm time missing from response: {response}"
        )


def _single_at_job(agent: Agent) -> tuple[str, datetime]:
    jobs = [
        line.strip()
        for line in agent.container.execute("atq").splitlines()
        if line.strip()
    ]
    assert len(jobs) == 1, f"expected one scheduled at job, found {len(jobs)}"

    match = re.fullmatch(r"(?P<id>\d+)\s+(?P<when>.+?)\s+\S\s+\S+", jobs[0])
    assert match is not None, "could not parse scheduled at job"
    scheduled = datetime.strptime(match.group("when"), "%a %b %d %H:%M:%S %Y")
    return match.group("id"), scheduled


def _container_today(agent: Agent) -> date:
    return date.fromisoformat(agent.container.execute("date", "+%F").strip())
