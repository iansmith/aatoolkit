"""Generate reference outputs for silero_vad.onnx over real telephony audio.

Companion to generate_goldens.py. Where that file uses 24 short frames (RNG-noise
"silence" + a speech fixture), this one runs the model over the full committed
`test_10s.ulaw` clip -- 312 frames of real 8 kHz mu-law telephony audio, state
threaded frame-to-frame -- long enough for the recurrent state to drift into the
regime that exposed the SOP-89 float32-exp overflow NaN. Run from this directory:

    pip install -r requirements.txt
    python generate_realaudio_goldens.py

The Go test decodes the same `test_10s.ulaw` with a byte-identical mu-law decoder,
so the fixture stores only per-frame reference probability + rms/peak (not the
decoded input, and not the recurrent state): the assertion is no-NaN and prob
agreement across all frames, which is what SOP-89 is about. Deterministic and
network-free; re-running rewrites realaudio_goldens.json.

onnxruntime's graph optimizer is disabled for the same reason as generate_goldens.py
(keep the reference close to the graph gonnx executes; take an ORT-version variable
out of the goldens). See PROVENANCE.md.
"""

import audioop
import hashlib
import json
import pathlib

import numpy as np
import onnxruntime as ort

HERE = pathlib.Path(__file__).parent
MODEL = HERE.parent / "onnx_models" / "silero_vad.onnx"
ULAW = HERE / "test_10s.ulaw"
GOLDENS = HERE / "realaudio_goldens.json"

SAMPLE_RATE = 8000
WINDOW = 256  # samples per inference frame at 8 kHz

MODEL_SHA256 = "1a153a22f4509e292a94e67d6f9b85e8deb25b4988682b7e174c65279d8788e3"


def checked_bytes(path: pathlib.Path, expected_sha256: str) -> bytes:
    raw = path.read_bytes()
    actual = hashlib.sha256(raw).hexdigest()
    if expected_sha256 and actual != expected_sha256:
        raise SystemExit(
            f"{path.name}: sha256 mismatch\n  expected {expected_sha256}\n  actual   {actual}\n"
            "Refusing to generate goldens from an unexpected input."
        )
    return raw


def main() -> None:
    model_raw = checked_bytes(MODEL, MODEL_SHA256)
    ulaw_raw = ULAW.read_bytes()

    pcm16 = audioop.ulaw2lin(ulaw_raw, 2)
    samples = np.frombuffer(pcm16, dtype="<i2").astype(np.float32) / 32768.0
    n = (len(samples) // WINDOW) * WINDOW
    frames = samples[:n].reshape(-1, WINDOW)

    so = ort.SessionOptions()
    so.graph_optimization_level = ort.GraphOptimizationLevel.ORT_DISABLE_ALL
    sess = ort.InferenceSession(str(MODEL), sess_options=so, providers=["CPUExecutionProvider"])

    state = np.zeros((2, 1, 128), dtype=np.float32)
    sr = np.array(SAMPLE_RATE, dtype=np.int64)

    out = []
    for i, fr in enumerate(frames):
        inp = fr.reshape(1, WINDOW).astype(np.float32)
        names = [o.name for o in sess.get_outputs()]
        d = dict(zip(names, sess.run(None, {"input": inp, "state": state, "sr": sr})))
        prob = float(np.asarray(d["output"]).ravel()[0])
        state = np.asarray(d["stateN"]).astype(np.float32)
        if not np.isfinite(prob) or not np.all(np.isfinite(state)):
            raise SystemExit(f"onnxruntime itself produced non-finite output at frame {i} -- unexpected")
        out.append({
            "index": i,
            "output": prob,
            "rms": float(np.sqrt(np.mean(fr.astype(np.float64) ** 2))),
            "peak": float(np.max(np.abs(fr))),
        })

    doc = {
        "source": "test_10s.ulaw",
        "source_sha256": hashlib.sha256(ulaw_raw).hexdigest(),
        "model_sha256": hashlib.sha256(model_raw).hexdigest(),
        "sample_rate": SAMPLE_RATE,
        "window_size": WINDOW,
        "state_shape": [2, 1, 128],
        "reference": "onnxruntime graph-opt-disabled",
        "frames": out,
    }
    GOLDENS.write_text(json.dumps(doc, indent=0))
    probs = [f["output"] for f in out]
    print(f"wrote {GOLDENS.name}: {len(out)} frames, prob range [{min(probs):.4f}, {max(probs):.4f}]")


if __name__ == "__main__":
    main()
