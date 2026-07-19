# aatoolkit

An open toolkit for building conversational AI assistants.

`aatoolkit` provides the reusable **mechanism** for an assistant that talks to people
over the phone and remembers what it learns:

- **Telephony** — Twilio media-stream handling, session management.
- **Speech** — speech-to-text and voice-activity detection.
- **LLM transport** — an OpenAI-compatible driver with tiered models and streaming.
- **Dynamic policy loading** — load and hot-reload assistant behavior written in Go, at
  runtime, so the policy can evolve without rebuilding the engine.
- **Fact-database toolkit** — extract facts from conversation and store them in a graph
  (Apache AGE / openCypher) with a vector store alongside.

It is **mechanism, not a finished assistant.** The particular behavior, identity, and
policy of a specific assistant live in a separate private repository that depends on
this module. Dependencies flow one way: private → `aatoolkit`, never the reverse.

## Status

Early. The reusable packages are being extracted from their original monorepo; the
public API will change until it settles. See `design/` for the architecture.

## License

MIT — see [LICENSE](LICENSE).
