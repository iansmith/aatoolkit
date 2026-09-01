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
  Whether an unknown caller is rejected before a turn starts is the server's policy, not
  twilio-cli's — a server that binds any caller accepts any well-formed E.164. twilio-cli
  only checks the shape. In voice mode `<FROM>` is optional and defaults with a warning;
  see [Voice](#voice). The `sms` subcommand always requires it.

## Voice

```bash
go run ./cmd/twilio-cli +15551234567
```

`<FROM>` is optional here. Omit it and the call dials from `+15105557890`, announcing the
substitution on one line so a defaulted number is never mistaken for one you chose:

```
twilio-cli: WARNING: no FROM given, defaulting source number to +15105557890
```

An explicit `<FROM>` is used verbatim and warns about nothing. The `sms` subcommand does
**not** default it: `sms` parses `<FROM> <BODY>` positionally, so defaulting the first
would make `twilio-cli sms "hello"` ambiguous between the two.

Posts the voice webhook, then opens the media-stream WebSocket and plays microphone audio in,
printing the server's marks and control frames as they arrive.

**The earcon cue means capture is live — not that the server is listening.** When capture
emits the mic-warm signal (the initial silence-discard period is over), twilio-cli plays a
240 ms 400 Hz tone to the local speaker. It fires within a second or two of the call
connecting, and it says one thing: your microphone is now being streamed, so a word spoken
from here on will not be clipped.

It does **not** say the server is ready for you. A server that opens with a recorded
introduction, or takes several seconds to generate its first response, is not listening
when this tone sounds — against a server whose introduction runs a minute or more, the cue
lands that far ahead of the caller's first real turn. Whatever signals "now it is your
turn" has to come from the server, over the call, because only the server knows. Treat
this tone as a capture check and wait for the server's own greeting.

This tone used to be a single 20 ms frame, which nobody could hear; it is now 240 ms with a
20 ms fade at each end (`earconDurationMS`, `earconRampMS` in `playback.go`).

| Flag | Purpose |
|---|---|
| `-webhook` | Full webhook URL. Skips config resolution entirely. |
| `-config` | Path to the config to resolve the target from. Overrides `$AATOOLKIT_TWILIO_CONFIG`. |
| `-server` | Name of the config server entry to resolve from. Overrides `$AATOOLKIT_SERVER_NAME`. |
| `-to` | The dialed number, E.164. |
| `-audio` | Stream a raw μ-law file instead of capturing the mic. Any platform. |
| `-no-echo-marks` | Suppress mark-echo, to exercise the server's `AwaitingMarkEcho` timeout. |
| `-record <file>` | Record inbound server audio to a raw μ-law file, with per-arrival timing in `<file>.jsonl`. |

### Pointing twilio-cli at a config

With no `-webhook`, the target is resolved from a config file **you** name — there is no
built-in default path. Give it with `-config <file>`, or export it once:

```bash
export AATOOLKIT_TWILIO_CONFIG=/absolute/path/to/your-fleet-config.toml
```

`-config` overrides the environment; `-webhook` overrides both and skips config resolution
entirely. Use an absolute path: twilio-cli is normally run from this checkout while the
config lives in the consuming project's, and a relative path resolves against your current
directory.

### Naming the server entry

Which entry in that config to read is likewise **yours** to name — there is no built-in
default name, for the same reason there is no default path. A fixed one would belong to
whichever consuming project happened to write it, and would stop resolving the day that
project renamed its entry, failing with an error naming a server you never configured.

```bash
export AATOOLKIT_SERVER_NAME=my-agent-server
```

`-server <name>` overrides the environment; `-webhook` overrides both and skips the lookup
entirely. The target is that entry's host at its **second** declared listen port — the
webhook port — so the entry needs two listens.

Both the path and the name apply to the `sms` subcommand too — same flags, same variables,
same precedence. The only difference is the route: `/webhook` for voice, `/sms/inbound`
for SMS.

### Recording what the server sent

`-record` answers the question a speaker cannot: **did the server's audio arrive?**

```bash
go run ./cmd/twilio-cli -record /tmp/call.ulaw +15551234567
```

It writes two files. `/tmp/call.ulaw` is the raw μ-law exactly as it came off the
socket — play it with `ffplay -f mulaw -ar 8000 /tmp/call.ulaw`. `/tmp/call.ulaw.jsonl`
carries one line per arrival with the offset into the call, the payload size, and the
gap since the previous payload:

```json
{"at_ms":100620,"bytes":160,"gap_ms":20,"total_bytes":804990}
```

The gaps are the useful part. A stream that stops for thirty seconds and resumes looks
identical to a continuous one in the audio file alone, and those gaps are precisely where
an operator reports hearing nothing. At the end of the call one summary line reports how
much sound arrived against how long the call ran:

```
recorded 812150 bytes of inbound audio (1m41.5s of sound over a 3m51s call, 0.44×)
```

A ratio well below 1 means the server genuinely sent silence for the rest of the call. A
ratio near 1 means the audio was there and anything unheard was lost after this process
received it.

### Streaming a file instead of the mic

Mic capture needs ffmpeg and is macOS-only (it uses avfoundation). `-audio` streams a raw
8 kHz μ-law file as the same 20 ms frames to the same endpoint, so it runs anywhere and is
deterministic — which makes it the way to drive the frame path from a script or a test:

```bash
go run ./cmd/twilio-cli -audio telephony/testdata/how_are_you.ulaw +15551234567
```

The repo's own fixtures work as input (`telephony/testdata/*.ulaw`, `telephony/assets/*.ulaw`).
Frames are paced in real time, not burst, so the server's VAD sees the clip exactly as it
would a live call. Reaching the end of the file ends the call the same way hanging up does.

One deliberate difference from the mic path: leading silence is **not** discarded — a fixture
streams verbatim, so replays stay reproducible. (The mic path discards up to 1500 ms of leading
silence while the microphone warms up; a file has no warm-up.)

The earcon **does** still sound, once, as the first frame goes out. It is the same mic-warm
signal, and there is nobody to cue on a file replay, so mute your output if you are running
unattended.

A relative `-audio` path resolves against your current directory, not the repository root.

The file must be **raw** μ-law with no container — a WAV will stream its 44-byte header as
audio. Convert with:

```bash
ffmpeg -i input.wav -ar 8000 -ac 1 -acodec pcm_mulaw -f mulaw out.ulaw
```

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
captured reply — From=+12183767443 To=+15551234567

...the reply text, with real line breaks...
```

| Flag | Purpose |
|---|---|
| `-capture-port` | Port the capture server binds. Default `9750`. Must match the `TWILIO_API_BASE_URL` the server was launched with. |
| `-webhook` | Full `/sms/inbound` URL. Skips config resolution. |
| `-config` | Path to the config to resolve the target from. Overrides `$AATOOLKIT_TWILIO_CONFIG`. |
| `-server` | Name of the config server entry to resolve from. Overrides `$AATOOLKIT_SERVER_NAME`. |
| `-to` | The Twilio number the SMS was addressed to, E.164. |

Do **not** set `TWILIO_API_BASE_URL` in your fleet config. Pointing the fleet at a local
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

  The value is an ffmpeg avfoundation device spec, `[video]:[audio]` — so the **leading colon
  matters**: `AATOOLKIT_STT_MIC=":1"` or `AATOOLKIT_STT_MIC=":USB Audio Device"`. A value with
  no colon is treated as the audio half and gets one prepended, so a bare `1` still works;
  passing a full spec such as `0:1` is left alone. List the devices ffmpeg can see with:

  ```bash
  ffmpeg -f avfoundation -list_devices true -i ""
  ```

For most testing scenarios, the small acoustic bleed is acceptable — speech will still
transcribe clearly.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `returned status 403` | `TWILIO_AUTH_TOKEN` doesn't match the server's, or the server was launched with `wss`. |
| `no "<name>" server declared in <your config>` | `-server`/`$AATOOLKIT_SERVER_NAME` names an entry your config does not declare. Fix the name, or pass `-webhook` explicitly. |
| `no server name: pass -server <name>, set AATOOLKIT_SERVER_NAME...` | No `-webhook` and no server name. Supply one of the three the message names. |
| `no config path: pass -config <file>, set AATOOLKIT_TWILIO_CONFIG...` | No `-webhook` and no config source. Supply one of the three the message names. |
| ffmpeg `Selected framerate ... is not supported` on mic capture | `AATOOLKIT_STT_MIC` named a **camera**: a bare value used to be read as the video device. Use the `:N` form. |
| `no reply reached the capture server within 35s` | The server was not launched with `TWILIO_API_BASE_URL` pointed at the capture port — it sent the reply to the real Twilio API instead. |
| `bind SMS capture server on port N` | Something else holds that port. Pick another with `-capture-port`, and launch the server with the matching `TWILIO_API_BASE_URL`. |
