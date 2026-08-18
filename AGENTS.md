# AGENTS.md

## Brainstorming Skill Modifications

When using the brainstorming skill, modify its behavior as follows.

### Question format

- Ask multiple questions at a time.
- Every question is multiple choice, with option **A** as the recommended answer.
- Only show options that are reasonable and have a solid case in their favor — anywhere from 2 to n options.
- If there are no reasonable options besides the recommended one, do not ask the question.

### Question budget

- Assume the user will only answer about **four questions** total.
- Lead with the most critical questions — the ones whose answers most reduce the risk that the resulting code is not useful, not correct, or makes harmful decisions.
- Think hard about getting the right four first.
- If the user answers and does not say stop, ask **four more**, again prioritized — they may be the last ones you get.
- If you run out of essential questions, tell the user you don't need to ask any more.

### Never ask what you can answer yourself

- Do not ask any question already answered by the user's previous statements or discoverable in the repo.
- Never ask a question you could answer yourself, even if it would take a lot of digging.

### Final report

- When finished (or when the user says to stop), summarize the plan on a single page.
- Describe the **product being built** — do not discuss architecture or implementation.
- The user will reply with a single approval or request for changes based on that report.
