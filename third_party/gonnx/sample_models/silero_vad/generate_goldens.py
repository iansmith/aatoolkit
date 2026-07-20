"""Generate reference outputs for silero_vad.onnx using onnxruntime.

These goldens are what the pure-Go gonnx runtime is checked against (SOP-80).
Run from this directory:

    pip install -r requirements.txt
    python generate_goldens.py

The output is deterministic: silence frames come from a fixed RNG seed, speech
frames from the committed speech_8k.raw fixture, and floats are serialized via
Python's repr (shortest round-trip). Re-running rewrites goldens.json
byte-identically, with no network access.

onnxruntime's graph optimizer is disabled defensively, not out of necessity: on
onnxruntime 1.27.0 every optimization level loads this model and yields bit-identical
outputs. Disabling it keeps the reference close to the graph gonnx executes and takes
an ORT-version variable out of the goldens. See PROVENANCE.md.

Why a real-speech fixture rather than a synthesized tone: Silero is trained to
reject non-speech. The obvious candidates -- sine stacks, chirps, white noise, AM
noise -- all score below 0.16, indistinguishable from silence. Goldens built from
them would exercise only one end of the model's output range while claiming to
cover speech. Run probe_synthetic_signals.py for the measurements.

Why an independently recorded fixture rather than Silero's own example audio, and
why it is put through a mu-law round-trip first: see PROVENANCE.md.
"""

import hashlib
import json
import pathlib

import numpy as np
import onnxruntime as ort

HERE = pathlib.Path(__file__).parent
MODEL = HERE.parent / "onnx_models" / "silero_vad.onnx"
SPEECH = HERE / "speech_8k.raw"
GOLDENS = HERE / "goldens.json"

SAMPLE_RATE = 8000
WINDOW = 256  # samples per inference frame at 8 kHz
SEED = 20260709

# Pin the inputs. Without these, a corrupted or swapped fixture would silently
# produce a plausible-looking goldens.json that no longer describes this model on
# this audio -- and the sha256 fields inside the file would agree with it, because
# they are computed from whatever was read.
MODEL_SHA256 = "1a153a22f4509e292a94e67d6f9b85e8deb25b4988682b7e174c65279d8788e3"
SPEECH_SHA256 = "a1d1cf9888cd5c6a7b513724b497fa7236ed66c588eecac2e095c22bae17515d"

# Leading silence, then speech, then trailing silence. The trailing run is the
# interesting part: Silero holds a high probability for a few frames after speech
# stops (its hangover), then decays. 16 frames takes it below 0.5 (at frame 14)
# and back down to the silence floor. That decay curve is a far stronger constraint
# on the recurrent state than a flat run would be -- a subtly wrong LSTM or If
# implementation drifts off it long before it disagrees on the speech frames.
LEADING_SILENCE = 3
TRAILING_SILENCE = 16


def checked_bytes(path: pathlib.Path, expected_sha256: str) -> bytes:
    raw = path.read_bytes()
    actual = hashlib.sha256(raw).hexdigest()
    if actual != expected_sha256:
        raise SystemExit(
            f"{path.name}: sha256 mismatch\n  expected {expected_sha256}\n  actual   {actual}\n"
            "Refusing to generate goldens from an unexpected input."
        )
    return raw


def silence_frame(rng: np.random.Generator) -> np.ndarray:
    return (rng.standard_normal(WINDOW) * 0.001).astype(np.float32)


def speech_frames(raw: bytes) -> list[np.ndarray]:
    samples = np.frombuffer(raw, dtype="<f4")
    if len(samples) % WINDOW:
        raise SystemExit(f"{SPEECH.name}: {len(samples)} samples is not a multiple of {WINDOW}")
    return [samples[i : i + WINDOW] for i in range(0, len(samples), WINDOW)]


def main() -> None:
    model_bytes = checked_bytes(MODEL, MODEL_SHA256)
    speech_bytes = checked_bytes(SPEECH, SPEECH_SHA256)

    opts = ort.SessionOptions()
    opts.graph_optimization_level = ort.GraphOptimizationLevel.ORT_DISABLE_ALL
    session = ort.InferenceSession(str(MODEL), opts, providers=["CPUExecutionProvider"])

    rng = np.random.default_rng(SEED)
    plan: list[tuple[str, np.ndarray]] = []
    plan += [("silence", silence_frame(rng)) for _ in range(LEADING_SILENCE)]
    plan += [("speech", f) for f in speech_frames(speech_bytes)]
    plan += [("silence", silence_frame(rng)) for _ in range(TRAILING_SILENCE)]

    state = np.zeros((2, 1, 128), dtype=np.float32)
    sr = np.array(SAMPLE_RATE, dtype=np.int64)

    frames = []
    for i, (kind, audio) in enumerate(plan):
        out, state = session.run(None, {"input": audio[None, :], "state": state, "sr": sr})
        frames.append(
            {
                "index": i,
                "kind": kind,
                "input": [float(v) for v in audio],
                "output": float(out[0][0]),
                # stateN is [2, 1, 128]; the batch axis is always 1 here, so store
                # it as 2 x 128 and let the reader add the axis back.
                "state_n": [[float(v) for v in state[layer][0]] for layer in range(2)],
            }
        )

    doc = {
        "model": "../onnx_models/silero_vad.onnx",
        "model_sha256": hashlib.sha256(model_bytes).hexdigest(),
        "speech_fixture_sha256": hashlib.sha256(speech_bytes).hexdigest(),
        "sample_rate": SAMPLE_RATE,
        "window_size": WINDOW,
        "seed": SEED,
        "state_shape": [2, 1, 128],
        "note": (
            "Generated by generate_goldens.py with onnxruntime and "
            "GraphOptimizationLevel.ORT_DISABLE_ALL. state_n is stored as 2x128; the "
            "batch axis (1) is elided. Each frame's state_n is the next frame's state; "
            "frame 0 starts from a zero state."
        ),
        "frames": frames,
    }

    GOLDENS.write_text(json.dumps(doc, indent=2) + "\n")

    speech = [f["output"] for f in frames if f["kind"] == "speech"]
    silence = [f["output"] for f in frames if f["kind"] == "silence"]
    print(f"wrote {GOLDENS.name}: {len(frames)} frames")
    print(f"  speech  min={min(speech):.4f} max={max(speech):.4f}")
    print(f"  silence min={min(silence):.4f} max={max(silence):.4f}")


if __name__ == "__main__":
    main()
