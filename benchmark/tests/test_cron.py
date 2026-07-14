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

        agent.new_session()
        response = agent("What jobs are currently scheduled with `at`?")

        response = response.lower()
        assert "mail" in response, f"mail job missing from response: {response}"
        assert "mom" in response, f"mom job missing from response: {response}"
        assert "4:45" in response, f"4:45pm time missing from response: {response}"
