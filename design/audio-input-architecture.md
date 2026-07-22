# Audio input architecture

aatoolkit has **one** audio ingestion path. Both real Twilio and the `twilio-cli`
fake-Twilio feed identical media frames to the same WebSocket endpoint; the
server-side sidecars (VAD, STT) take over from there. Local *interactive* input is
typed-only. There is no second, local-microphone audio path — an earlier one was
removed (see History) precisely so there would be a single VAD and a single STT.

```
  real phone call ─┐
                   ├─▶ μ-law frames ─▶ /streams ─▶ telephony session ─▶ VAD ─▶ STT ─▶ Turn ─▶ Respond
  twilio-cli ──────┘        (WebSocket)              (Silero sidecar) (whisper sidecar)   (the seam)

  typed line ─▶ stdinSource (driver) ───────────────────────────────────────────▶ Turn ─▶ Respond
```

## The one audio path: frames → `/streams` → sidecars

- **Real Twilio** streams call audio as 8 kHz μ-law 20 ms frames over a Media
  Streams WebSocket to `/streams` (`telephony/twilio`).
- **`twilio-cli`** is a byte-faithful fake-Twilio: `streamMicFrames`
  (`cmd/twilio-cli/capture_darwin.go`) captures the local mic via ffmpeg, slices it
  into the same 8 kHz μ-law 20 ms frames, and `websocket.Dial`s the same `/streams`
  endpoint. That is *why* it is a faithful test tool — it is indistinguishable from
  Twilio at the frame boundary.
- **Server side**, the telephony session runs VAD (Silero, externalized to a sidecar
  by default — SOP-145) and STT (`telephony.STTClient.Transcribe` → the whisper
  sidecar at `/v1/audio/transcriptions`). This is the single VAD and the single STT
  client in the engine.

So the answer to "how do I get local voice in?" is **run `twilio-cli`** — it streams
mic frames to the same endpoint and gets the same sidecar VAD/STT. Do not add a
second capture/VAD path.

## Local interactive input: typed only

`driver` keeps a small typed-input seam for the REPL / `/reload` dev loop:

```go
type Turn struct{ Text, Speaker string } // Speaker empty until speaker-ID lands

type inputSource interface {
    Next() (Turn, bool, error) // ok=false = end of input (EOF / quit)
}
```

`stdinSource` is the one implementation: it reads and trims typed lines. It carries
no VAD and no audio — it duplicates nothing in the frame path. The seam is currently
unexported and not yet wired into a production run loop (WIP).

## History: the removed local-mic path

An earlier local path recorded from the microphone *inside* the driver and ran its
**own** VAD and STT — separate from the telephony pipeline:

- `captureUtterance` — ffmpeg/avfoundation mic capture
- an `endpointer` state machine driven by ffmpeg `silencedetect` (a second VAD)
- a `voiceSource` input source wiring the two together
- `driver.transcribe` — a second whisper HTTP client (posted WAV; the telephony
  client posts μ-law)
- `cmd/vadspike` — a throwaway harness for tuning that VAD

This was removed (2026-07-22) as a redundant parallel path: `twilio-cli` already
streams mic frames to `/streams`, so the local mic case is served by the frame
endpoint, and keeping a second VAD + second STT client violated the one-mechanism
rule. If local voice input is ever wanted without a full phone call, extend
`twilio-cli` (or the frame path), not a separate in-driver capture/VAD.

## The convergence seam

Both inputs exist to produce the same thing: a user text `Turn` bound for the
policy's `Respond`. The frame path yields it via STT through the turn sink; the typed
path yields it directly from `stdinSource`. Unifying these onto one `Respond` entry
point is the "Turn seam" work.
