from itertools import cycle, islice

from walle_bench import Agent, Client, timeout

MAX_TURNS = 25
TIMEOUT = 120  # seconds


def test_onboarding():

    stop_word = "hotcakes"
    client = Client(f"""\
        Your name is Matt. You are an personal tax accountant at Acme Accounting.
        matt@acme.com is your email address. Acme Accounting is a small accounting
        firm in Winnipeg. 

        You are setting up an AI agent (named Ava) to help you with your work. Say "hi" to kick
        off the onboarding process. The agent should guide you through the setup.

        When done, please respond with the word '{stop_word}' to indicate that conversation is done.
        """)

    response = None
    with timeout(TIMEOUT), Agent() as agent:
        for bot in islice(cycle([client, agent]), MAX_TURNS):
            response = bot(response)

            if stop_word in response and bot is client:
                break
        else:
            raise RuntimeError("Exceeded max turns")

    context_md = (agent.workspace / "CONTEXT.md").read_text()

    assert "Matt" in context_md
    assert "Ava" in context_md
    assert "Acme Accounting" in context_md
    assert "Winnipeg" in context_md
    assert "<to-be-deleted>" not in context_md

    # agent.clean()
    print(agent.workspace)


def runner():
    try:
        test_onboarding()
        print(f"{test_onboarding.__name__} passed")
    except Exception as e:
        import traceback

        traceback.print_exc()
        print(f"Test failed: {e}")


if __name__ == "__main__":
    from threading import Thread

    threads = [
        Thread(target=runner),
        Thread(target=runner),
        Thread(target=runner),
    ]

    for t in threads:
        t.start()

    for t in threads:
        t.join()
