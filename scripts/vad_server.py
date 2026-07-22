#!/usr/bin/env python3
"""FastAPI + ONNX Runtime sidecar for Silero VAD (voice activity detection).

Accepts POST requests of an input window (320 float32: 64 context + 256 chunk)
followed by a 256 float32 state vector, runs inference on the model, and returns
the speech probability + updated state (1028 bytes total). It's a sibling of the
whisper (STT) and
supertonic (TTS) sidecars: a separate process the Go driver talks to over HTTP,
so the driver never links the model.

Launched by the fleet supervisor. Config via CLI flags (env vars are fallback
defaults, kept for other launchers):
  --host / AATOOLKIT_VAD_HOST   bind address (default: 127.0.0.1)
  --port / AATOOLKIT_VAD_PORT   bind port    (default: 7790)
  AATOOLKIT_VAD_MODEL           path to silero_vad.onnx (default: models/silero_vad/silero_vad.onnx)

Warmed on startup so /healthz cannot answer until the model is loaded and
executes a first inference, following the voice_server.py / whisper_server.py
pattern.
"""

import os
import struct
import time
from contextlib import asynccontextmanager

import numpy as np
import uvicorn
from fastapi import FastAPI, Request
from fastapi.responses import Response
import onnxruntime as ort

import sidecar

MODEL = os.environ.get("AATOOLKIT_VAD_MODEL", "models/silero_vad/silero_vad.onnx")

# Silero VAD's I/O contract — the Go side names the same values in
# aatoolkit/telephony/silero.go. At 8kHz the model input is a 64-sample context
# prepended to the 256-sample chunk = 320 samples (AATK-8; feeding a bare 256 runs
# the model cold and misses short utterances). The client owns the context, so this
# sidecar stays stateless: it just runs whatever input width it receives.
WINDOW_SIZE = 256
CONTEXT_SIZE = 64
INPUT_SIZE = CONTEXT_SIZE + WINDOW_SIZE  # 320
STATE_SHAPE = (2, 1, 128)
STATE_BYTES = 2 * 1 * 128 * 4  # 1024 — flattened float32 state, always the request's tail
SAMPLE_RATE = np.array(8000, dtype=np.int64)


def build_session(model_path: str) -> ort.InferenceSession:
    """Build and return an ONNX Runtime session for the VAD model."""
    return ort.InferenceSession(model_path, providers=["CPUExecutionProvider"])


def create_app(*, warmup: bool = True) -> FastAPI:
    """Build the FastAPI app for the VAD sidecar.

    Args:
        warmup: Build the session and run a first inference in the lifespan
            hook, before /healthz can answer. Tests pass False — they only
            inspect routes, and should not load the ONNX model.
    """

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        if warmup:
            print(f"vad: building session from {MODEL} ...", flush=True)
            t0 = time.monotonic()
            app.state.session = build_session(MODEL)
            print("vad: warming model ...", flush=True)
            app.state.session.run(None, {
                "input": np.zeros((1, INPUT_SIZE), dtype=np.float32),
                "state": np.zeros(STATE_SHAPE, dtype=np.float32),
                "sr": SAMPLE_RATE,
            })
            print(f"vad: ready in {time.monotonic() - t0:.1f}s", flush=True)
        yield

    app = FastAPI(lifespan=lifespan)
    sidecar.add_healthz(app)

    @app.post("/")
    async def process(request: Request):
        """Process one VAD inference request.

        Accepts input (N float32 little-endian) ++ state (256 float32). The state is
        always the trailing STATE_BYTES; the input is whatever precedes it — INPUT_SIZE
        (320: 64 context + 256 chunk) in production, but any width the model accepts.

        Returns 1028 bytes:
        - First 4 bytes: speech probability (float32 little-endian)
        - Next 1024 bytes: updated state (256 float32, [2,1,128] flattened C-order)
        """
        data = await request.body()
        if len(data) <= STATE_BYTES or (len(data) - STATE_BYTES) % 4 != 0:
            return Response(status_code=400)

        # frombuffer/reshape are zero-copy views over the request bytes. Input is the
        # leading (len - STATE_BYTES) bytes; state is the trailing STATE_BYTES.
        split = len(data) - STATE_BYTES
        input_window = np.frombuffer(data[:split], dtype=np.float32).reshape((1, split // 4))
        state = np.frombuffer(data[split:], dtype=np.float32).reshape(STATE_SHAPE)

        outputs = request.app.state.session.run(None, {
            "input": input_window,
            "state": state,
            "sr": SAMPLE_RATE,
        })
        prob = outputs[0]        # shape (1, 1)
        new_state = outputs[1]   # shape (2, 1, 128), float32, C-contiguous

        # Response: 4-byte probability + 1024-byte C-order state. tobytes()
        # already serializes C-order, so no flatten is needed.
        response = struct.pack("<f", float(prob.item())) + new_state.tobytes()
        return Response(content=response, media_type="application/octet-stream")

    return app


# Module-level app for uvicorn and for tests that only inspect routes, matching
# whisper_server.app — keeps vad_server.app working as launched. (voice_server
# has no module-level app: its create_app() would eagerly build the TTS model.)
app = create_app()


def parse_args(argv=None):
    return sidecar.build_arg_parser(__doc__, port=7790, env_prefix="VAD").parse_args(argv)


if __name__ == "__main__":
    args = parse_args()
    uvicorn.run(app, host=args.host, port=args.port)
