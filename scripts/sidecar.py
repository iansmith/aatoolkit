"""Shared scaffolding for aatoolkit's voice-path FastAPI sidecars.

voice_server.py (TTS), whisper_server.py (STT), and vad_server.py (VAD) are all
the same shape: a separate process the Go driver talks to over HTTP, each
exposing a repo-owned ``GET /healthz`` and taking ``--host``/``--port``. This
module holds the two pieces they share so the convention lives in one place.

Health contract (one definition for all three): ``/healthz`` returns a constant
``200 {"status": "ok"}``. Readiness is gated *structurally*, not by this
response — each sidecar runs its model warm-up inside the ASGI lifespan, which
uvicorn completes before it accepts a single connection, so the supervisor's
readiness poll sees connection-refused until the server is warm and 200 only
once it is. A reachable /healthz is therefore already warm.
"""
import argparse
import os

from fastapi import FastAPI


def add_healthz(app: FastAPI) -> None:
    """Register the shared ``GET /healthz`` readiness probe on ``app``.

    Constant 200 by design — see the module docstring for why readiness is
    gated by the lifespan rather than by this response body.
    """

    @app.get("/healthz")
    def healthz():
        return {"status": "ok"}


def build_arg_parser(
    description: str, *, port: int, env_prefix: str, host: str = "127.0.0.1"
) -> argparse.ArgumentParser:
    """Build the shared ``--host``/``--port`` parser.

    The supervisor auto-appends ``--host``/``--port`` from the fleet config; the
    ``AATOOLKIT_<PREFIX>_HOST`` / ``AATOOLKIT_<PREFIX>_PORT`` env vars are fallback
    defaults for other launchers. ``env_prefix`` names each sidecar's namespace
    (STT, VAD, TTS); ``port`` is that sidecar's default port.
    """
    parser = argparse.ArgumentParser(description=description)
    parser.add_argument(
        "--host",
        default=os.environ.get(f"AATOOLKIT_{env_prefix}_HOST", host),
        help="bind address (default: %(default)s)",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=int(os.environ.get(f"AATOOLKIT_{env_prefix}_PORT", str(port))),
        help="bind port (default: %(default)s)",
    )
    return parser
