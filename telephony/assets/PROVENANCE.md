# Vendored asset provenance (the engine copy)

This directory holds `go:embed`-ded binary assets: the farewell clip and two
sound-effect clips (SOP-156). All are documented below.

---

# audio-forced-stop.ulaw / llm-thinking.ulaw — sound effects (SOP-156)

| | audio-forced-stop | llm-thinking |
|---|---|---|
| Embedded file | `audio-forced-stop.ulaw` | `llm-thinking.ulaw` |
| Source (kept) | `audio-forced-stop.mp3` | `llm-thinking.mp3` |
| Format | headerless μ-law, 8 kHz, mono (`len/8000` == seconds) | same |
| μ-law size | 12672 bytes = 1.58s | 40320 bytes = 5.04s |
| Role | played when a single utterance exceeds `MaxUtteranceMS` and the caller is cut off, then the call terminates via the mark-echo flow | a loopable "thinking" bed for a future feature; **embedded but wired to no playback path in SOP-156** |
| Original filename | `daviddumaisaudio-sci-fi-weapon-laser-shot-04-316416.mp3` | `soundzee-futuristic-ui-beeps-356129.mp3` |
| License | **Pixabay Content License** (deferred: verify attribution requirements before production; swap if terms are unclear — Ian's call, 2026-07-17) | same |

Unlike `farewell.ulaw`, these are downloaded effects with no regeneration
script, so the source `.mp3` is kept alongside the embedded `.ulaw`. Convert with:

```sh
ffmpeg -i <name>.mp3 -ar 8000 -ac 1 -f mulaw <name>.ulaw
```

---

# farewell.ulaw — call-termination clip

| | |
|---|---|
| File | `farewell.ulaw` |
| Format | headerless μ-law, 8 kHz, mono, 1 byte/sample (`len/8000` == seconds) |
| Text | `Call me anytime... ... Bye!` |
| Source | voice-out (supertonic) TTS, `POST /v1/tts` |
| Voice | `F5` (built-in style; supertonic ships M1–M5, F1–F5) |
| Speed | 0.8 (supertonic's own default is 1.05, which reads as rushed) |
| Post-processing | leading/trailing silence trimmed, 8 ms fade at each edge |
| Shipped clip | 16644 bytes = 2.08s, 0.42s internal pause |

Regenerate (requires voice-out up — `aa-server-status> voice-out up`):

```sh
scripts/make_farewell.sh --play        # audition  -> build/farewell.ulaw
scripts/make_farewell.sh --install     # copy into this directory
go build -o build/server ./cmd/server  # re-embed
```

## Why the text has an ellipsis

The pause between "anytime" and "Bye!" comes from punctuation, not from
supertonic's `silence_duration` parameter. That parameter pads only *between
chunks*, and each chunk is synthesized with its own ~0.5s of lead/trail
silence — so the smallest gap it can produce is ~0.8s, and the concatenation
seam is a waveform discontinuity that clicks audibly (measured: a 19.5%-of-
full-scale sample jump, versus 12% for a single-chunk clip). An ellipsis keeps
the line in one chunk: no seam, no click, and the pause lands where the text
says it should.

## Why it is trimmed

The model pads every clip with silence. The trailing pad is the one that
costs: the engine sends the clip, waits out a mark echo timed from the clip's byte
length, then hangs up — so trailing silence is dead line the caller sits
through after "Bye!". Trimming cuts mid-waveform (the threshold is crossed on
a rising edge, not at a zero crossing), which leaves a non-zero first sample
and an audible click; the 8 ms fades force both edges to zero.

## Not reproducible byte-for-byte

Supertonic's synthesis is not seeded, so re-running `make_farewell.sh` yields a
slightly different rendering (duration varies by ~5%) — there is no sha256 to
pin here. Audition after regenerating.

