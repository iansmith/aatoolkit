# aatoolkit

An open Go engine for building voice and telephony AI agents.

`aatoolkit` is the **mechanism** of a phone agent: it answers the call, relays the audio to
a speech-to-speech backend, drives the LLM, and keeps track of what it learns. A specific
agent supplies the **meaning** — prompts, persona, policy, tools, ontology — from its own
repository. Any number of agents can run on the same engine.

```
go get github.com/iansmith/aatoolkit
```

## Key features

| Area | Package | What it gives you |
|---|---|---|
| **Telephony** | `telephony/twilio` | Twilio voice webhooks, signature validation, Media Streams over WebSocket, inbound SMS and outbound SMS via the REST API. |
| **Speech-to-speech relay** | `telephony/realtime`, `telephony/twilio` | Bridges a phone call to any OpenAI-Realtime-compatible voice backend, with no transcoding. See [below](#speech-to-speech). |
| **LLM driver** | `driver`, `host` | OpenAI-compatible LLM client with tiered models and streaming, an OpenTelemetry span per model request (see [Tracing](#tracing)), TTS synthesis and a serial speech queue, health and Twilio HTTP servers. |
| **Hot-reloadable policy** | `interp` | Loads agent policy written in plain Go at runtime (via [yaegi](https://github.com/traefik/yaegi)) against a host interface you choose. Change behavior without rebuilding or redeploying the engine. |
| **Fact extraction** | `extract` | Wire contract for an entity-extraction sidecar (e.g. GLiNER), for turning conversation into stored facts. |
| **Process supervisor** | `cmd/aa-server-status` | Launches, health-checks, and tears down the engine and its model sidecars from one TOML config; interactive REPL or `--auto up\|down` for systemd. |
| **Test tooling** | `cmd/twilio-cli` | Drive a local server as if it were Twilio — voice or SMS, no account or phone needed. |

## How an agent plugs in

The engine never imports the agent. The agent injects itself through three seams:

1. **`driver.Config`** — model tiers, TTS transport, and a system-prompt provider.
2. **`interp.Load`** — the directory of Go policy source to interpret, plus the symbols it may import.
3. **`host.Host`** — the interface the interpreted policy calls back into (LLM transport by tier).

Dependencies flow one way: agent → `aatoolkit`, never the reverse.

## Speech-to-speech

`aatoolkit` hands each call to an external **speech-to-speech** backend that speaks the OpenAI Realtime WebSocket protocol — for
example the open-source [speech-to-speech](https://github.com/iansmith/speech-to-speech)
server (a fork of [huggingface/speech-to-speech](https://github.com/huggingface/speech-to-speech)),
which runs a fully local, swappable VAD → STT → LLM → TTS stack behind that protocol.

The engine is a thin, careful relay between the phone carrier and the backend:

- **One entry point.** `twilio.NewStreamHandler(realtimeURL, opts...)` routes calls to the backend.
  There is no built-in pipeline to fall back to: an empty URL ends each call with an error.
  Consumers that own their handler can call `twilio.HandleStreamRealtime` directly.
- **No transcoding.** Twilio's base64 G.711 μ-law goes to the backend as-is, and the reply
  audio comes back the same way. Barge-in is forwarded as a carrier `clear`.
- **Per-call identity.** `WithInstructions[For]` sets the persona, `WithVoice` the voice,
  `WithTools` the tool schemas (passed through byte-for-byte), and `WithSessionID[For]` a
  session id the speech-to-speech server forwards on each LLM request as `X-Client-Session-Id`, so a proxy can
  tie requests to the call.
- **Observe everything.** Channels for transcripts (`WithTranscriptChan`), every server event
  (`WithServerEventChan`), inbound caller audio, and every payload sent to the carrier.
- **Steer mid-call.** Send your own client events (e.g. tool results) with
  `WithClientEventChan`, change the voice with `WithVoiceUpdateChan`, and use
  `WithMarkRequestChan` / `WithMarkEchoChan` to learn when the caller has actually *heard*
  a reply.
- **Call hygiene.** `WithFillerAudio` plays a hold loop while the model is thinking or
  running a tool, `WithIdleTimeout` ends calls whose backend has gone quiet, and
  `WithFarewellAudio` plays a goodbye before hanging up. `WithCallerSilenceTimeout` ends a
  call once the caller has said nothing for that long after the agent's reply finished
  playing, with the farewell first and its own error (`ErrCallerSilenceTimeout`); pair it
  with `WithIdleTimeout`, which covers a call where the agent never speaks.

The **backend's** VAD and turn-taking run the conversation; the engine has no turn-taking
of its own.

```go
handler := twilio.NewStreamHandler("ws://localhost:8765/v1/realtime",
    twilio.WithInstructions("You are a friendly receptionist."),
    twilio.WithIdleTimeout(30*time.Second),
)
```

## Tracing

The driver starts one OpenTelemetry CLIENT span for each model call it makes. The engine never
installs a provider, exporter or propagator: install your SDK and the spans appear. Wired end
to end, an inbound SMS's model call nests under the webhook's server span:

```go
otel.SetTracerProvider(tp)
h := driver.New(cfg)

srv.HandleSMS = func(ctx context.Context, m twilio.InboundSMS) {
    go policyTurn(h.WithContext(context.WithoutCancel(ctx)), m.Body)
}
driver.StartTwilioServer(addr, srv, driver.WithHandlerWrapper(
    func(route string, next http.Handler) http.Handler {
        if route == "/streams" {
            return next // a /streams span would last the whole call
        }
        return otelhttp.NewHandler(next, route,
            otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }))
    }))
```

- **Provider.** A nil `Config.TracerProvider` (the default) means the global provider, looked
  up at each call, so installing it before or after `driver.New` both work. With nothing
  installed, the no-op provider records nothing. Setting `driver.Config{TracerProvider: tp}`
  instead reaches the driver alone: otelhttp still uses the global provider unless it is given
  `otelhttp.WithTracerProvider(tp)`, so without that the webhook span goes unrecorded and the
  model span becomes a root.
- **Context.** `host.ContextSender` adds `SendContext` and `SendStreamContext`, which take a
  `context.Context`: its span parents the model span, and its deadline and cancellation bound
  the call (the driver's own 300 s ceiling still applies). A call ctx cuts short fails with an
  error that matches both `ctx.Err()` and `context.Cause(ctx)` under `errors.Is`, wherever the
  call was when it stopped and over HTTP/1 or HTTP/2 alike, so a ctx cancelled with a cause
  matches that cause as well as `context.Canceled`. `Send` and `SendStream` run under
  `context.Background()`, so their spans are roots. `h.WithContext(ctx)` returns a `host.Host`
  whose `Send`, `SendStream` and `Context` run under `ctx`; make one per inbound request and do
  not keep it.
- **User-context provider.** `driver.Config.UserContextCtx` is the ctx-taking form of
  `UserContext`; if both are set, `UserContextCtx` wins and `UserContext` is never called.
  `host.ContextBuilder` adds `ContextFor(ctx, queryText)`, which hands ctx on unchanged to that
  provider so its I/O can run under the caller's span, deadline and cancellation; the driver
  adds none of its own, and a plain `UserContext` never sees ctx. `Context(queryText)` hands it
  `context.Background()`, and a `WithContext` view's `Context` hands it the bound ctx.
  `ContextFor` panics on a nil ctx. The provider's contract is on
  `driver.Config.UserContext` and `driver.Config.UserContextCtx`.
- **Request-scoped ctx.** The ctx a Twilio handler receives (`HandleSMS`, `HandleSMSStatus`,
  `RejectCall`, `AnswerCall`) is cancelled once the request has been handled. A model call that
  outlives the handler must run under `context.WithoutCancel(ctx)`, which keeps the span and
  drops the cancellation; under the raw ctx it fails with `context.Canceled`. The same holds for
  a `UserContextCtx` provider handed that ctx.
- **Interpreted policies.** (R1) Change nothing in the policy or its `interp.Inject`: hand it
  `h.WithContext(ctx)` wherever it is handed `h` today; its `Send`, `SendStream` and `Context`
  then run under `ctx`. (R2) Give the policy entrypoint a ctx parameter, inject
  `"ContextSender": reflect.ValueOf((*host.ContextSender)(nil))` and
  `"ContextBuilder": reflect.ValueOf((*host.ContextBuilder)(nil))` beside `"Host"` for the host
  package, and build and send with
  `if cb, ok := h.(host.ContextBuilder); ok { window = cb.ContextFor(ctx, q) } else { window = h.Context(q) }` and
  `if cs, ok := h.(host.ContextSender); ok { cs.SendStreamContext(ctx, ...) }`. An interface
  the policy declares itself cannot stand in for the injected one.
- **Webhook spans.** `WithHandlerWrapper` wraps each Twilio route (`/webhook`, `/streams`,
  `/sms/inbound`, `/sms/status`) before it is mounted. On `/streams` the writer passed on must
  keep `http.Hijacker` (otelhttp's does), or the upgrade fails with 501. The wrapper runs
  before signature validation: it must not read `r.Body` or rewrite `r.Host` or `r.URL`, and
  forged requests get spans too. The engine adds no attributes of its own there, never From
  or To.
- **Message content.** Off by default, because it is personal data. Setting
  `AATOOLKIT_CAPTURE_MESSAGE_CONTENT` to a value `strconv.ParseBool` reads as true (`1`,
  `true`) records each request's messages and the model's output as `gen_ai.input.messages`
  and `gen_ai.output.messages`; unset, empty, false or unparsable is off. Values are not
  truncated: bound them with the SDK's span limits (`OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT`).
- **Token usage.** Recorded whenever the stream carries it. `Tier.IncludeUsage` also sends
  `"stream_options": {"include_usage": true}`; it is off by default, and off leaves the request
  body unchanged. Measured on one hosted OpenAI-compatible provider: it accepts the key, and it
  also sends a final usage chunk without it. A local mlx-serve has not been measured, so keep
  `IncludeUsage` off on local tiers until it is.

The span is named `chat {Tier.Model}` (`chat` when the model is empty), with scope
`github.com/iansmith/aatoolkit/driver` and schema `https://opentelemetry.io/schemas/1.41.0`.
It covers the whole call, `onSegment` time included. An unknown tier, or a window that does not
encode (bytes that are not JSON, or an empty non-nil slice), gets no span and sends nothing; a
nil window encodes as `null` and is sent, as before. A call that sends nothing because its
context is already done, or because its tier URL cannot be requested, still gets a span, which
records why it failed.

| Attribute | Value |
|---|---|
| `gen_ai.operation.name` | `chat` |
| `gen_ai.provider.name` | `openai`, the wire format; `server.address` tells upstreams apart |
| `gen_ai.request.model` | `Tier.Model`, when set |
| `gen_ai.request.stream` | `true` |
| `gen_ai.request.max_tokens` | `Tier.MaxTokens`, when above 0 |
| `server.address`, `server.port` | the tier URL's host, and its port or the scheme's default; neither when the URL has no host |
| `aatoolkit.llm.tier` | the tier name the caller passed |
| `gen_ai.response.model`, `gen_ai.response.id` | the first non-empty values the stream sent |
| `gen_ai.response.finish_reasons` | the stream's finish reason, or `["error"]` for a failure that had none |
| `gen_ai.response.time_to_first_chunk` | seconds from sending to the first data chunk |
| `aatoolkit.llm.time_to_first_content` | seconds from sending to the first non-empty content; later than the first chunk on role-first upstreams and reasoning tiers |
| `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens` | the stream's token usage, when it carries any |
| `error.type` | on failure, with status Error and an empty description: the upstream's HTTP status (`429`, ...), `timeout`, `canceled`, `transport_error`, `upstream_error`, `decode_error`, `stream_read_error`, `empty_response`, or `_OTHER` (a panicking `onSegment`) |
| `gen_ai.input.messages`, `gen_ai.output.messages` | only with message content capture on |

Never on a span: the API key or any request header; URL userinfo, path or query; error text or
upstream error bodies. The engine adds no From or To attribute anywhere, but with message
content capture on, `gen_ai.input.messages` and `gen_ai.output.messages` hold whatever the
messages carry: the system prompt, and any number or name the consumer put in the window. The
span has no events, and no trace headers are added to the upstream request.

## Status

Early and moving. The public API will change until it settles. See [`design/`](design/)
for architecture notes.

## License

MIT — see [LICENSE](LICENSE).
