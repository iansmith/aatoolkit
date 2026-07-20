# Silero VAD — vendored model provenance

## The model

| | |
|---|---|
| File | `../onnx_models/silero_vad.onnx` |
| Upstream repo | https://github.com/snakers4/silero-vad |
| Upstream path | `src/silero_vad/data/silero_vad.onnx` |
| Version | **v6.2.1** (see "Which version is this?" below) |
| Size | 2,327,524 bytes |
| sha256 | `1a153a22f4509e292a94e67d6f9b85e8deb25b4988682b7e174c65279d8788e3` |
| License | MIT — Copyright (c) 2020-present Silero Team |
| ONNX opset | 16 |

Verify the vendored copy at any time:

```sh
shasum -a 256 third_party/gonnx/sample_models/onnx_models/silero_vad.onnx
# 1a153a22f4509e292a94e67d6f9b85e8deb25b4988682b7e174c65279d8788e3
```

## Which version is this?

**It is v6.2.x, not v5**, despite what the tickets and design docs say. Verified on
2026-07-10 by downloading `src/silero_vad/data/silero_vad.onnx` from each upstream tag and
comparing sha256:

| upstream tag | sha256 (first 8) | same bytes as ours? |
|---|---|---|
| `v5.0` | `6b99cbfd` | no (also a different size: 2,313,101 bytes) |
| `v5.1.1` | `2623a295` | no |
| `v5.1.2` | `2623a295` | no |
| `v6.0` | `597d30b3` | no |
| `v6.1` | `597d30b3` | no |
| **`v6.2`** | **`1a153a22`** | **yes** — first tag with these bytes |
| **`v6.2.1`** | **`1a153a22`** | **yes** — latest tag, identical to v6.2 |
| `master` @ 2026-07-10 | `1a153a22` | yes |

Pin to **`v6.2.1`**: it is the newest tag carrying these exact bytes, and `master` is a
moving target. `v6.2` is byte-identical and equally valid.

Note that `v5.0` also lives at a different upstream path (`files/silero_vad.onnx`); the
`src/silero_vad/data/` layout arrived later.

Everything the gonnx work targets — 689 nodes, 25 distinct operators, 25 `If` nodes nested
to depth 4, 2 reflect-mode `Pad` nodes, 4 `Size` nodes, 4 LSTMs — was measured against
*this* file, so the analysis and the operator work are unaffected by the mislabelling. Only
the version string was wrong.

## Model interface

Inputs:

| name | shape | dtype | notes |
|---|---|---|---|
| `input` | `[batch, samples]` | float32 | 256 samples per frame at 8 kHz |
| `state` | `[2, batch, 128]` | float32 | zeros to start; thread `stateN` back in |
| `sr` | `[]` (rank-0) | int64 | `8000` or `16000` |

Outputs:

| name | shape | dtype | notes |
|---|---|---|---|
| `output` | `[batch, 1]` | float32 | speech probability in [0, 1] |
| `stateN` | `[2, batch, 128]` | float32 | feed into the next frame's `state` |

the engine runs 8 kHz (Twilio's native μ-law rate), so no resampling is needed.

## The speech fixture

| | |
|---|---|
| File | `speech_8k.raw` — 1280 samples (0.16 s), little-endian float32, 5,120 bytes |
| Source | An **independent** recording, made for this repo. Not supplied by Silero. |
| Content | One Harvard sentence: *"The birch canoe slid on the smooth planks."* Phonetically balanced, no personal content. |
| Chain | 48 kHz mono capture → 8 kHz mono → G.711 μ-law encode → decode → float32 |
| Slice | the 5 consecutive frames with the highest minimum probability |
| sha256 | `a1d1cf9888cd5c6a7b513724b497fa7236ed66c588eecac2e095c22bae17515d` |

**Why not Silero's own example audio.** The obvious fixture is `examples/c++/aepyx_8k.wav`,
which ships with the model. It was used initially and then rejected: audio distributed
*with* a model is plausibly in that model's training or tuning set, so any confidence drawn
from it about whether the VAD detects real speech is circular.

That circularity does **not** affect the goldens' primary job — SOP-80 compares gonnx
against onnxruntime on identical inputs, so where the audio came from cannot mask a gonnx
bug. It matters for the separate question of whether the model works on speech we did not
get from the vendor. Measured, on this model:

| fixture | source | speech-frame probabilities |
|---|---|---|
| `aepyx_8k.wav` | Silero's own, clean 8 kHz | 0.94, 0.96, 0.97, 0.97, 0.96 |
| this fixture | independent, μ-law round-trip | 0.83, 0.94, 0.93, 0.97, 0.96 |

Both rows are the five speech frames **as generated in `goldens.json`** — that is, entered
from the recurrent state left by the three leading silence frames. (Scored from a zero
state instead, the same audio reads 0.96–0.99 and 0.80–0.98 respectively; the numbers move
because the state differs, which is the whole reason the goldens thread it.)

An unrelated voice, degraded through μ-law, scores comparably to the vendor's demo file.
The model is not merely recognising its own example.

**Why the μ-law round-trip.** the engine's VAD reads a Twilio Media Stream: 8 kHz G.711 μ-law.
Running the fixture through encode/decode reproduces the quantisation noise the model will
actually see in production. The old fixture was clean 8 kHz and never exercised it.

**Why the source recording is not committed.** This repo is public. The committed artifact
is the 0.16-second slice, pinned by the sha256 above and by `SPEECH_SHA256` in
`generate_goldens.py`, which refuses to run if the fixture does not match. The full
7-second recording is intentionally omitted. Consequently the *slice* cannot be re-derived
from an upstream source the way the old one could — the fixture itself is the artifact of
record. Regenerating `goldens.json` needs only the fixture, and remains fully reproducible.

**Why a real-speech fixture and not a synthesized tone.** Silero is trained to reject
non-speech. Measured against this model — reproduce with `python probe_synthetic_signals.py`,
which defines each signal exactly:

| signal | peak | mean |
|---|---|---|
| near-silence (σ = 0.001 noise) | 0.154 | 0.050 |
| white noise (σ = 0.3) | 0.100 | 0.064 |
| AM-modulated noise (4 Hz) | 0.063 | 0.034 |
| harmonic stack 140/280/700 Hz | 0.002 | 0.001 |
| glottal pulse train → formants | 0.012 | 0.005 |
| chirp 300→900 Hz | 0.008 | 0.001 |

All sit at or below the silence floor, while the vendored slice scores **0.94–0.97**.

This is not a claim that *no* synthetic signal can read as speech — the space is large, and
other harmonic stacks do cross 0.5. It is the narrower, sufficient point: the obvious
candidates do not, so goldens built from them would exercise only one end of the model's
output range while claiming to cover speech. Real audio removes the guesswork.

## Goldens

`goldens.json` holds reference outputs produced by onnxruntime, for `gonnx` to be checked
against (SOP-80). 24 frames: 3 of silence, 5 of speech, then 16 more of silence.

That trailing run is the valuable part. Silero holds a high probability for a few frames
after speech stops — its hangover — and only then decays, crossing below 0.5 at frame 14
and settling back to the silence floor (0.097). A subtly wrong LSTM or `If` implementation
will drift off that curve long before it disagrees about the speech frames themselves.

```
frame   0    1    2 |  3    4    5    6    7 |  8    9   10   11   12   13   14 ...  23
kind    silence     |  speech               |  silence
prob  .19  .24  .15 | .83  .94  .93  .97  .96 | .96  .92  .89  .82  .71  .57  .45 ... .10
                                                                        cross 0.5 ^
```

Regenerate with (no network needed — both the model and the speech fixture are vendored):

```sh
pip install -r requirements.txt
python generate_goldens.py
```

Regeneration is byte-identical: silence frames come from a fixed RNG seed, speech frames
from the committed fixture, and floats are serialized with Python's shortest round-trip
repr.

The generator pins `GraphOptimizationLevel.ORT_DISABLE_ALL`. On the pinned toolchain
(onnxruntime 1.27.0) this is **defensive, not required**: verified that `ORT_ENABLE_BASIC`,
`ORT_ENABLE_EXTENDED` and `ORT_ENABLE_ALL` all load this model and produce bit-identical
outputs across all 24 frames (max abs diff 0.0). Disabling optimization keeps the reference
as close as possible to the graph gonnx actually executes, and removes an ORT-version
variable from the goldens.

Do not confuse this with the *offline specialization* attempt, which is a different thing
and genuinely does fail. Pinning the input shapes and asking ORT to export an optimized
model (`optimized_model_filepath`) errors out, as does `onnxsim` (segfault). That is why
`If` had to be implemented in the gonnx executor rather than folded away, and why the model
is vendored unmodified. Those failures belong to graph rewriting, not to plain inference.
