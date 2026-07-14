from walle_bench import Agent, seed, static, timeout

TIMEOUT = 120  # seconds


def test_recognizes_cat_image():
    with timeout(TIMEOUT), Agent() as agent:
        seed("contexts/minimal.md", agent.workspace / "CONTEXT.md")

        response = agent(
            "What is in the attached image?",
            attachments=[static / "photo.jpg"],
        )

    assert any(word in response.lower() for word in ["cat", "kitten"]), f"{response=}"
