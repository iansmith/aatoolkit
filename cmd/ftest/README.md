# ftest — LLM fact-extraction probe

Measures how well the local model (gemma, the "fast" tier that talks to customers)
pulls **storable facts** out of everyday conversation: entities, reified temporal
facts, placeholders for unknown people, and — the thing we most care about — whether
it flags high-value gaps (names, familial relationships) for an **immediate**
follow-up. Background: `docs/fact-database-research.md`.

The model emits **structured JSON** (not Cypher); a deterministic compiler turns that
JSON into Apache AGE openCypher. That split keeps "did it understand the facts"
measurable independently of "did it emit valid Cypher".

## Build

```
go build ./cmd/ftest
```

## Endpoint

Talks OpenAI format straight to mlx-serve on port 1234 (NOT the LiteLLM
Claude→OpenAI shim on 1235 — ftest doesn't need it):

```
AATOOLKIT_FTEST_URL   (default http://127.0.0.1:1234/v1/chat/completions)
AATOOLKIT_FTEST_MODEL (default mlx-community/gemma-4-31b-it-8bit)
```

mlx-serve loads models on demand by the `model` field, so point AATOOLKIT_FTEST_MODEL
at a reasoning model (e.g. ornith) on the same endpoint to compare.

## Record — generate a fixture by talking

```
ftest record -name birthdays -o cmd/ftest/fixtures/01-birthdays.json
```

Type a user turn; each turn is extracted **incrementally** (all prior turns + known
entity handles as context) and the JSON is printed. Prefix a line with `a:` to add
an assistant turn for context. `/done` or EOF saves the fixture (turns + per-turn
extraction). Then hand-write a `gold` block to turn it into a scored test.

## Run — replay fixtures and grade

```
ftest run cmd/ftest/fixtures/*.json          # extract + grade against gold
ftest run -cypher cmd/ftest/fixtures/02-choir.json   # also print AGE openCypher
```

`run` re-extracts every user turn live, consolidates the per-turn results into one
conversation-level view, compiles it to Cypher, and (when the fixture has `gold`)
scores it:

- **statement recall** — of the gold facts, how many did the model find (matched on
  predicate + value/object, case-insensitive; surface wording ignored).
- **immediate follow-up recall** — of the gaps gold says to ask *now*, how many did
  the model flag `immediate`.

## Fixture shape

```jsonc
{
  "name": "birthdays",
  "notes": "what makes this case interesting",
  "turns": [
    {"speaker": "assistant", "text": "..."},   // context, not extracted
    {"speaker": "user", "text": "..."}          // extracted
  ],
  "gold": { "entities": [...], "statements": [...], "followups": [...] }
}
```

`gold` is optional — without it, `run` prints the extraction but doesn't score.
Seed examples (the three canonical utterances) live in `fixtures/`.

## What "good" looks like

The probe is answering three questions about gemma specifically:
1. Does it find the multiple facts packed into one turn?
2. Does it leave unknown people as placeholders (not hallucinate names)?
3. Does it flag names / familial relationships as `immediate` — the follow-ups
   the assistant should voice while the context is fresh — and bank the rest?
