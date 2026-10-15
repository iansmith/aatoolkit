# aatoolkit

An open Go engine for building voice and telephony AI agents.

`aatoolkit` is the **mechanism** of a phone agent: it answers the call, moves the audio,
decides when the caller has finished speaking, drives the LLM, and keeps track of what it
learns. A specific agent supplies the **meaning** — prompts, persona, policy, tools,
ontology — from its own repository. Any number of agents can run on the same engine.

```
go get github.com/iansmith/aatoolkit
```

## Key features

| Area | Package | What it gives you |
|---|---|---|
| **Telephony** | `telephony/twilio` | Twilio voice webhooks, signature validation, Media Streams over WebSocket, inbound SMS and outbound SMS via the REST API. |
| **Speech-to-speech relay** | `telephony/realtime`, `telephony/twilio` | Bridges a phone call to any OpenAI-Realtime-compatible voice backend, with no transcoding. See [below](#speech-to-speech). |
| **Speech pipeline** | `telephony` | Silero VAD, an STT router, and a turn-taking state machine that decides when the caller is done — with tunable silence/utterance/turn caps. |
| **LLM driver** | `driver`, `host` | OpenAI-compatible LLM client with tiered models and streaming, TTS synthesis and a serial speech queue, health and Twilio HTTP servers. |
| **Hot-reloadable policy** | `interp` | Loads agent policy written in plain Go at runtime (via [yaegi](https://github.com/traefik/yaegi)) against a host interface you choose. Change behavior without rebuilding or redeploying the engine. |
| **Fact extraction** | `extract` | Wire contract for an entity-extraction sidecar (e.g. GLiNER), for turning conversation into stored facts. |
| **Process supervisor** | `cmd/aa-server-status` | Launches, health-checks, and tears down the engine and its model sidecars from one TOML config; interactive REPL or `--auto up\|down` for systemd. |
| **Test tooling** | `telephony` replay, `cmd/probeset`, `cmd/twilio-cli` | Replay recorded calls through the turn-taking logic, record per-decision event logs, and drive a local server as if it were Twilio — voice or SMS, no account or phone needed. |

## How an agent plugs in

The engine never imports the agent. The agent injects itself through three seams:

1. **`driver.Config`** — model tiers, TTS transport, and a system-prompt provider.
2. **`interp.Load`** — the directory of Go policy source to interpret, plus the symbols it may import.
3. **`host.Host`** — the interface the interpreted policy calls back into (LLM transport by tier).

Dependencies flow one way: agent → `aatoolkit`, never the reverse.

## Speech-to-speech

Besides its own VAD → STT → LLM → TTS pipeline, `aatoolkit` can hand a call to an external
**speech-to-speech** backend that speaks the OpenAI Realtime WebSocket protocol — for
example the open-source [speech-to-speech](https://github.com/iansmith/speech-to-speech)
server (a fork of [huggingface/speech-to-speech](https://github.com/huggingface/speech-to-speech)),
which runs a fully local, swappable VAD → STT → LLM → TTS stack behind that protocol.

In this mode the engine is a thin, careful relay between the phone carrier and the backend:

- **One switch.** `twilio.NewStreamHandler(realtimeURL, opts...)` routes calls to the backend;
  an empty URL keeps the built-in pipeline. Consumers that own their handler can call
  `twilio.HandleStreamRealtime` directly.
- **No transcoding.** Twilio's base64 G.711 μ-law goes to the backend as-is, and the reply
  audio comes back the same way. Barge-in is forwarded as a carrier `clear`.
- **Per-call identity.** `WithInstructions[For]` sets the persona, `WithVoice` the voice,
  `WithTools` the tool schemas (passed through byte-for-byte), and `WithSessionID[For]` a
  session id the backend forwards on each LLM request as `X-Client-Session-Id`, so a proxy can
  tie requests to the call.
- **Observe everything.** Channels for transcripts (`WithTranscriptChan`), every server event
  (`WithServerEventChan`), inbound caller audio, and every payload sent to the carrier.
- **Steer mid-call.** Send your own client events (e.g. tool results) with
  `WithClientEventChan`, change the voice with `WithVoiceUpdateChan`, and use
  `WithMarkRequestChan` / `WithMarkEchoChan` to learn when the caller has actually *heard*
  a reply.
- **Call hygiene.** `WithFillerAudio` plays a hold loop while the model is thinking or
  running a tool, `WithIdleTimeout` ends calls whose backend has gone quiet, and
  `WithFarewellAudio` plays a goodbye before hanging up.

Note that on this path the **backend's** VAD and turn-taking run the conversation;
the engine's own turn-taking logic is bypassed for those calls.

```go
handler := twilio.NewStreamHandler("ws://localhost:8765/v1/realtime",
    twilio.WithInstructions("You are a friendly receptionist."),
    twilio.WithIdleTimeout(30*time.Second),
)
```

## Status

Early and moving. The public API will change until it settles. See [`design/`](design/)
for architecture notes.

## License

MIT — see [LICENSE](LICENSE).
