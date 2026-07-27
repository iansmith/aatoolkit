# twilio-cli

Drives a locally-running agent server as if it were Twilio — a real voice call or a real
inbound SMS, without a Twilio account, a phone, or a public tunnel. It performs the same
signed-webhook ceremony Twilio does, so the server cannot tell the difference.

## Prerequisites

- **`TWILIO_AUTH_TOKEN` exported, matching the server's.** Every webhook is signed over the
  exact URL posted to. A mismatch is a `403` with no other explanation.
- **Launch the server with the `ws` stream scheme, not `wss`.** Answering `wss` makes every
  signature fail with a silent 403.
- **`<FROM>` must be a number the server's roster knows**, in E.164 (`+15551234567`).
  Unknown callers are rejected before a turn starts.

## Voice

```bash
go run ./cmd/twilio-cli +15551234567
```

Posts the voice webhook, then opens the media-stream WebSocket and plays microphone audio in,
printing the server's marks and control frames as they arrive.

**Speaking after the earcon cue:** When capture emits the mic-warm signal (indicating the
initial silence-discard period is over), twilio-cli plays a 400Hz earcon tone to the local
speaker to signal "capture is live — speak now." This allows the operator to know exactly
when the call is ready to record speech without relying on blind timing. Speak promptly
*after* the cue so the full opening word is captured.

| Flag | Purpose |
|---|---|
| `-webhook` | Full webhook URL. Skips config resolution entirely. |
| `-to` | The dialed number, E.164. |
| `-no-echo-marks` | Suppress mark-echo, to exercise the server's `AwaitingMarkEcho` timeout. |

With no `-webhook`, the target is resolved from `aa-server-status.toml`: the server named
`"the server"`, at its second declared listen port. If your config names it something else,
resolution fails and you must pass `-webhook` explicitly.

## SMS — two steps, in this order

The reply to an inbound SMS is **not** in the webhook response. The server answers the webhook
in milliseconds with empty TwiML, queues the turn, and delivers the reply out-of-band over the
Twilio REST API. So the CLI runs a local capture server that stands in for `api.twilio.com`,
and waits for the reply to land on it (up to 35s).

The server reads `TWILIO_API_BASE_URL` **once at startup**, so it has to be launched already
pointed at the capture port. That is why the order matters and cannot be reversed:

```bash
export TWILIO_API_BASE_URL=http://127.0.0.1:9750
```

Launch the server from that shell, then:

```bash
go run ./cmd/twilio-cli sms -capture-port 9750 +15551234567 "what's on today?"
```

On success it prints the reply it intercepted:

```
capture server listening on port 9750 — launch the server with TWILIO_API_BASE_URL=http://127.0.0.1:9750
captured reply: To=+15551234567 Body="..."
```

| Flag | Purpose |
|---|---|
| `-capture-port` | Port the capture server binds. Default `9750`. Must match the `TWILIO_API_BASE_URL` the server was launched with. |
| `-webhook` | Full `/sms/inbound` URL. Skips config resolution. |
| `-to` | The Twilio number the SMS was addressed to, E.164. |

Do **not** set `TWILIO_API_BASE_URL` in `aa-server-status.toml`. Pointing the fleet at a local
capture server by default would silently stop real SMS replies from being sent.

## Acoustic bleed mitigation

The earcon tone is played through the local speaker and can be heard in the background
by the microphone, creating a small amount of acoustic "bleed" into the recording. This is
not a software isolation issue (capture and playback are separate OS processes), but a
physical acoustics effect.

To eliminate acoustic bleed, use one of these mitigation strategies:

- **Headphones:** Connect headphones to the speaker output. The microphone will not pick up
  headphone audio.
- **Separate output device:** Set `AATOOLKIT_STT_MIC` to point to an alternate audio input
  (e.g. a USB headset microphone instead of the system mic), while the earcon plays through
  built-in speakers.

For most testing scenarios, the small acoustic bleed is acceptable — speech will still
transcribe clearly.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `returned status 403` | `TWILIO_AUTH_TOKEN` doesn't match the server's, or the server was launched with `wss`. |
| `no "the server" server declared in aa-server-status.toml` | Your config names the server something else. Pass `-webhook` explicitly. |
| `no reply reached the capture server within 35s` | The server was not launched with `TWILIO_API_BASE_URL` pointed at the capture port — it sent the reply to the real Twilio API instead. |
| `bind SMS capture server on port N` | Something else holds that port. Pick another with `-capture-port`, and launch the server with the matching `TWILIO_API_BASE_URL`. |
