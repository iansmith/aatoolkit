"""Handler test for scripts/vad_server.py (SOP-148).

Drives the real FastAPI app in-process — the starlette TestClient runs the
lifespan, which builds the ONNX Runtime session against the real vendored model
— and asserts that ``POST /`` reproduces the wire fixture's response byte-for-
byte for every frame in both golden sets. Real ORT, real model; the fixture
(SOP-146, telephony/testdata/silero_wire_fixture.json) is the oracle.

Run: .venv/bin/python -m pytest scripts/test_vad_server.py
"""
import json
import os
import pathlib

HERE = pathlib.Path(__file__).parent  # aatoolkit/scripts/
REPO = HERE.parent  # aatoolkit/
MODEL = REPO / "models" / "silero_vad" / "silero_vad.onnx"
FIXTURE = REPO / "telephony" / "testdata" / "silero_wire_fixture.json"

# vad_server reads AATOOLKIT_VAD_MODEL at import time; point it at the vendored
# model with an absolute path so the test is CWD-independent. Must precede the
# import below.
os.environ["AATOOLKIT_VAD_MODEL"] = str(MODEL)

from fastapi.testclient import TestClient  # noqa: E402
import vad_server  # noqa: E402


def _fixture_frames():
    """(set_name, index, request_hex, response_hex) for every frame in the fixture."""
    fixture = json.loads(FIXTURE.read_text())
    for set_name in ("meetings_today", "synthetic"):
        for frame in fixture[set_name]["frames"]:
            yield set_name, frame["index"], frame["request_hex"], frame["response_hex"]


def test_handler_reproduces_wire_fixture():
    frames = list(_fixture_frames())
    assert frames, "wire fixture has no frames"

    # The handler is stateless — the recurrent state travels in each request, so
    # every fixture frame can be POSTed independently and must reproduce its
    # response byte-for-byte (real ONNX Runtime, real model).
    with TestClient(vad_server.app) as client:
        for set_name, index, request_hex, response_hex in frames:
            resp = client.post(
                "/",
                content=bytes.fromhex(request_hex),
                headers={"Content-Type": "application/octet-stream"},
            )
            assert resp.status_code == 200, f"{set_name}[{index}]: status {resp.status_code}"
            assert resp.content.hex() == response_hex, (
                f"{set_name}[{index}]: response bytes do not match the fixture"
            )
