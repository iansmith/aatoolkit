"""Generate onnxruntime reference per-frame speech probabilities for
`meetings_today.ulaw` (SOP-96): a real recording of "Can you show me my
meetings today?", 8 kHz mono G.711 mu-law, 10.0 s.

Adapted from
third_party/gonnx/sample_models/silero_vad/generate_realaudio_goldens.py — same
mu-law decode, windowing, and recurrent-state threading, retargeted at this
ticket's own fixture and output file (this script writes into
internal/telephony/testdata/, not the third_party fork's directory).

Run from this directory:

    pip install onnxruntime==1.27.0 numpy==2.4.6  # requires Python < 3.13 (audioop removed in 3.13)
    python generate_goldens.py

Deterministic and network-free (the model is vendored, the recording is
committed): re-running rewrites meetings_today_goldens.json byte-for-byte
identically. onnxruntime's graph optimizer is disabled so the reference stays
close to the graph gonnx actually executes (see the fork's PROVENANCE.md for
why — verified bit-identical across optimization levels for this model).
"""

import audioop
import hashlib
import json
import pathlib

import numpy as np
import onnxruntime as ort

HERE = pathlib.Path(__file__).parent
# testdata/ -> telephony/ -> aatoolkit/ . The engine no longer vendors the model
# (SOP-149 removed both third_party/gonnx and the telephony/assets embed), so this
# dev-only regenerator expects it supplied out-of-tree at models/silero_vad/silero_vad.onnx
# (the sidecar's default path); the sha256 check below rejects any other file. The
# committed fixtures make regeneration a rare, offline step.
MODEL = HERE.parent.parent / "models" / "silero_vad" / "silero_vad.onnx"
ULAW = HERE / "meetings_today.ulaw"
GOLDENS = HERE / "meetings_today_goldens.json"
SYNTHETIC = HERE / "silero_goldens.json"
WIRE_FIXTURE = HERE / "silero_wire_fixture.json"

SAMPLE_RATE = 8000
WINDOW = 256  # samples per inference chunk at 8 kHz — the Silero window size
CONTEXT = 64  # samples of the previous chunk prepended as context (AATK-8); model input is CONTEXT+WINDOW=320

MODEL_SHA256 = "1a153a22f4509e292a94e67d6f9b85e8deb25b4988682b7e174c65279d8788e3"
RECORDING_SHA256 = "556fe18f83e69ddd021a19d353a45f1352e4cec9a3ed78aa2e2aa7973c7cdc03"


def checked_bytes(path: pathlib.Path, expected_sha256: str) -> bytes:
    raw = path.read_bytes()
    actual = hashlib.sha256(raw).hexdigest()
    if expected_sha256 and actual != expected_sha256:
        raise SystemExit(
            f"{path.name}: sha256 mismatch\n  expected {expected_sha256}\n  actual   {actual}\n"
            "Refusing to generate goldens from an unexpected input."
        )
    return raw


def wire_frames(sess, windows) -> list:
    """Replay 256-sample windows through the VAD sidecar's exact wire contract and
    return per-frame {index, request_hex, response_hex}.

    This mirrors telephony/silero_http.go byte-for-byte (AATK-8): each request is the
    64-sample carried context ++ the 256-sample window (= 320 float32) followed by the
    current [2,1,128] recurrent state (256 float32), all little-endian; each response is
    the speech probability (1 float32) followed by the new state (256 float32). Context
    and state are threaded frame-to-frame exactly as the Go detector threads them, both
    zero at frame 0. So a request is 320*4 + 256*4 = 2304 bytes (4608 hex chars) and a
    response is 4 + 256*4 = 1028 bytes (2056 hex chars).
    """
    names = [o.name for o in sess.get_outputs()]
    sr = np.array(SAMPLE_RATE, dtype=np.int64)
    state = np.zeros((2, 1, 128), dtype=np.float32)
    context = np.zeros(CONTEXT, dtype=np.float32)
    out = []
    for i, w in enumerate(windows):
        w = np.asarray(w, dtype=np.float32)
        inp = np.concatenate([context, w]).astype(np.float32)  # 320 samples
        request = inp.astype("<f4").tobytes() + state.astype("<f4").tobytes()
        d = dict(zip(names, sess.run(None, {"input": inp.reshape(1, CONTEXT + WINDOW), "state": state, "sr": sr})))
        prob = np.asarray(d["output"], dtype=np.float32).reshape(-1)[:1]
        state = np.asarray(d["stateN"], dtype=np.float32)
        context = w[-CONTEXT:].astype(np.float32)
        response = prob.astype("<f4").tobytes() + state.astype("<f4").tobytes()
        out.append({"index": i, "request_hex": request.hex(), "response_hex": response.hex()})
    return out


def main() -> None:
    model_raw = checked_bytes(MODEL, MODEL_SHA256)
    ulaw_raw = checked_bytes(ULAW, RECORDING_SHA256)

    pcm16 = audioop.ulaw2lin(ulaw_raw, 2)
    samples = np.frombuffer(pcm16, dtype="<i2").astype(np.float32) / 32768.0
    n = (len(samples) // WINDOW) * WINDOW
    frames = samples[:n].reshape(-1, WINDOW)

    so = ort.SessionOptions()
    so.graph_optimization_level = ort.GraphOptimizationLevel.ORT_DISABLE_ALL
    sess = ort.InferenceSession(str(MODEL), sess_options=so, providers=["CPUExecutionProvider"])

    state = np.zeros((2, 1, 128), dtype=np.float32)
    sr = np.array(SAMPLE_RATE, dtype=np.int64)
    context = np.zeros(CONTEXT, dtype=np.float32)  # AATK-8: previous chunk's tail, zero-init

    out = []
    for i, fr in enumerate(frames):
        inp = np.concatenate([context, fr]).reshape(1, CONTEXT + WINDOW).astype(np.float32)
        names = [o.name for o in sess.get_outputs()]
        d = dict(zip(names, sess.run(None, {"input": inp, "state": state, "sr": sr})))
        prob = float(np.asarray(d["output"]).ravel()[0])
        state = np.asarray(d["stateN"]).astype(np.float32)
        context = fr[-CONTEXT:].astype(np.float32)  # carry this chunk's tail forward
        if not np.isfinite(prob) or not np.all(np.isfinite(state)):
            raise SystemExit(f"onnxruntime itself produced non-finite output at frame {i} -- unexpected")
        out.append({"index": i, "output": prob})

    doc = {
        "source": "meetings_today.ulaw",
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

    # --- wire fixture (SOP-146) ---------------------------------------------
    # The byte-exact request/response oracle for the HTTP detector's tests (SOP-147):
    # a separate file, generated from the same real ONNX Runtime inference, over both
    # the real-audio (meetings_today) and synthetic (silero_goldens) window sets.
    synthetic = json.loads(SYNTHETIC.read_text())
    synthetic_windows = [f["input"] for f in synthetic["frames"]]
    wire = {
        "model_sha256": hashlib.sha256(model_raw).hexdigest(),
        "wire": "request = input(context+window = 320 f32) ++ state(256 f32); "
        "response = prob(1 f32) ++ stateN(256 f32); little-endian throughout. "
        "Matches telephony/silero_http.go (AATK-8 320-sample input).",
        "meetings_today": {"frames": wire_frames(sess, frames)},
        "synthetic": {"frames": wire_frames(sess, synthetic_windows)},
    }
    WIRE_FIXTURE.write_text(json.dumps(wire, indent=0))
    print(
        f"wrote {WIRE_FIXTURE.name}: meetings_today={len(wire['meetings_today']['frames'])} "
        f"synthetic={len(wire['synthetic']['frames'])} frames"
    )


if __name__ == "__main__":
    main()
