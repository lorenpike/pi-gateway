from textwrap import dedent

import jmespath

from walle_bench import Agent, static, timeout

TIMEOUT = 120  # seconds


def test_remembers_favorite_colour():
    prompt = dedent("""\
        I am working on a website for a customer. The file is at
        ~/projects/harry/tax-bracket.html. Right now the header is shade of blue,
        but I want to change it to #FFBD2E because it is my favorite colour.
        """)

    with timeout(TIMEOUT), Agent(clean=True) as agent:
        (agent.workspace / "CONTEXT.md").write_text(
            (static / "contexts" / "carl.md").read_text()
        )

        project = agent.workspace / "projects" / "harry"
        project.mkdir(parents=True, exist_ok=True)
        (project / "tax-bracket.html").write_text(
            (static / "projects" / "tax-brackets.html").read_text()
        )

        _ = agent(prompt)  # We care about the tools called, not the response

        edits_made = jmespath.search(
            "[].message.content[] | [?type == 'toolCall'] | [?name == 'edit']  | [].arguments.edits[].newText",
            agent.transcript,
        )
        assert "#FFBD2E" in "\n".join(edits_made), f"{edits_made=}"

        agent.new_session()  # Start fresh
        response = agent("What is my favorite colour?")

        assert "#FFBD2E" in response, f"{response=}"


if __name__ == "__main__":
    test_remembers_favorite_colour()
