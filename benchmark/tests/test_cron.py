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
    assert "rio" in response, f"Rio cron job missing from response: {response}"
    assert "weather" in response, f"weather cron job missing from response: {response}"
    assert "hour" in response, f"hourly schedule missing from response: {response}"
