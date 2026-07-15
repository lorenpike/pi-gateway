from walle_bench import Agent, seed, timeout

TIMEOUT = 120  # seconds


def test_no_reply_for_file_request():
    with timeout(TIMEOUT), Agent() as agent:
        seed("contexts/minimal.md", agent.workspace / "CONTEXT.md")
        seed("photo.jpg", agent.workspace / "cat.jpg")

        response = agent("There is a ~/cat.jpg, can you send it to me in this channel")

    assert "NO_REPLY" == response.strip(), f"{response=}"
