#!/usr/bin/env python3
"""
FastAPI + uvicorn sidecar for NER-based fact extraction using GLiNER.

Implements the extraction wire contract from AATK-51. Binds to 127.0.0.1,
with model path/repo-id passed as a command-line flag and resolved at startup.
No app is built at module import (see sidecar.py comment: "voice_server has no
module-level app: its create_app() would eagerly build the TTS model.").

**Model identity:** --model defaults to os.environ.get("AATOOLKIT_FACTS_MODEL",
"urchade/gliner_small-v2.1"). The sidecar classifies the value by one stateless
rule, with no filesystem probing:

* A value starting with `.`, `/` or `~` is a **local filesystem path**. `~`
  expands against the user's home; a **relative** value resolves against the
  repository root (the parent of `scripts/`), never against the process working
  directory, because `config.Server.Dir` lets the supervisor launch this child
  with an arbitrary cwd.
* Anything else is a **Hugging Face repo id**, passed to `from_pretrained`
  unchanged and resolved from the local cache. `urchade/gliner_small-v2.1`
  takes this branch: it contains a `/` but does not start with `.`, `/` or `~`.

The distinction is **deliberate** — `models/gliner` is read as a repo id while
`./models/gliner` is read as a path. This keeps the rule free of filesystem
guessing.

**No runtime egress:** Before the `gliner` import, the module sets
`os.environ.setdefault("HF_HUB_OFFLINE", "1")` and
`os.environ.setdefault("TRANSFORMERS_OFFLINE", "1")`, so a mis-typed model id
can never silently become a download. The model must already be in the local HF
cache, **and so must its base encoder** `microsoft/deberta-v3-small`, which the
checkpoint's config references for the tokenizer. A cache miss is a loud startup
failure, which is the intended behaviour.

**Lazy heavy import:** `import gliner` happens inside `load_model`, never at
module scope. Consequences: --help and argument errors are instant, and
`scripts/test_facts_server.py` runs in an environment that has `fastapi` but
not `gliner`.

**Operator note:** This sidecar needs `fastapi`, `uvicorn`, and `gliner` (which
pulls in `torch` and `transformers`). A fleet `[[server]]` entry should list
them in its `packages = [...]` array.
"""

import os
import sys
from contextlib import asynccontextmanager
from pathlib import Path

os.environ.setdefault("HF_HUB_OFFLINE", "1")
os.environ.setdefault("TRANSFORMERS_OFFLINE", "1")

import uvicorn
from fastapi import FastAPI
from fastapi.responses import JSONResponse

import sidecar


def char_span_to_byte_span(text: str, char_start: int, char_end: int) -> tuple[int, int]:
    """
    Convert character offsets to UTF-8 byte offsets.

    Args:
        text: The text string.
        char_start: Character offset for the span start.
        char_end: Character offset for the span end.

    Returns:
        (byte_start, byte_end) tuple.
    """
    byte_start = len(text[:char_start].encode("utf-8"))
    byte_end = len(text[:char_end].encode("utf-8"))
    return byte_start, byte_end


def resolve_model(value: str) -> str:
    """
    Resolve a model identifier to either an absolute filesystem path or a Hugging Face repo id.

    Classification rule (stateless, no filesystem probing except existence check):
    * If value starts with `.`, `/`, or `~`: treat as a filesystem path.
      - `~` expands against the user's home directory.
      - Relative paths resolve against the repository root (parent of `scripts/`).
      - The resolved directory must exist; if not, exit with code 2.
    * Otherwise: return unchanged as a Hugging Face repo id.

    Args:
        value: The model identifier (path or repo id).

    Returns:
        The absolute resolved path (for filesystem paths) or repo id unchanged.

    Exits:
        With code 2 and stderr message if a path-shaped value doesn't exist.
    """
    # Detect path-shaped values
    if value.startswith(("..", "/", "~")) or value.startswith("."):
        # Expand ~ first
        expanded = Path(value).expanduser()

        # If it's absolute after expansion, use it
        if expanded.is_absolute():
            resolved = expanded
        else:
            # Relative path: resolve against repo root
            repo_root = Path(__file__).resolve().parent.parent
            resolved = repo_root / expanded

        # Normalize to remove ./ segments
        resolved = resolved.resolve()

        # Check existence
        if not resolved.exists():
            print(f"facts: model path not found: {resolved}", file=sys.stderr)
            sys.exit(2)

        return str(resolved)

    # Not path-shaped: return as repo id unchanged
    return value


def load_model(model_id: str):
    """
    Load a GLiNER model from a filesystem path or Hugging Face repo id.

    This function performs the lazy import of `gliner`, which is why it lives
    inside a function and not at module scope.

    Args:
        model_id: Either an absolute filesystem path or a Hugging Face repo id.

    Returns:
        A gliner.GLiNER model instance.
    """
    import gliner

    return gliner.GLiNER.from_pretrained(model_id)


def parse_args(argv=None):
    parser = sidecar.build_arg_parser(__doc__, port=7791, env_prefix="FACTS")
    parser.add_argument(
        "--model",
        default=os.environ.get("AATOOLKIT_FACTS_MODEL", "urchade/gliner_small-v2.1"),
        help="Model identifier (Hugging Face repo id or local path). Default: urchade/gliner_small-v2.1",
    )
    return parser.parse_args(argv)


def create_app(*, model=None, warmup=True, extractor=None):
    """
    Create a FastAPI app for the extraction sidecar.

    Seams for testing:
    * `extractor`: When given, use it as the model and skip loading. (Test seam)
    * `warmup`: When False, skip loading in lifespan even without an extractor.
    * `model`: The model to load in lifespan (only if extractor=None and warmup=True).

    Args:
        model: Model id or path to pass to load_model during lifespan.
        warmup: Whether to load the model in the lifespan.
        extractor: A pre-built extractor (for testing).

    Returns:
        A FastAPI application.
    """
    @asynccontextmanager
    async def lifespan(app: FastAPI):
        if warmup and extractor is None:
            app.state.extractor = load_model(model)
        yield

    app = FastAPI(title="extraction-sidecar", lifespan=lifespan)
    if extractor is not None:
        app.state.extractor = extractor
    sidecar.add_healthz(app)

    @app.post("/extract")
    async def extract(request_body: dict):
        """
        Extract named entities from text using the GLiNER model.

        Request body:
        {
            "text": str (required),
            "labels": [str, ...] (required, non-empty),
            "threshold": float in [0, 1] (required)
        }

        Response:
        {
            "spans": [
                {
                    "text": str,
                    "label": str,
                    "start": int (UTF-8 byte offset),
                    "end": int (UTF-8 byte offset),
                    "score": float
                },
                ...
            ]
        }
        """
        # Validate request
        if "text" not in request_body:
            return JSONResponse(status_code=400, content={"error": "missing text"})
        if "labels" not in request_body:
            return JSONResponse(status_code=400, content={"error": "missing labels"})
        if "threshold" not in request_body:
            return JSONResponse(status_code=400, content={"error": "missing threshold"})

        text = request_body["text"]
        labels = request_body["labels"]
        threshold = request_body["threshold"]

        # Validate types
        if not isinstance(text, str):
            return JSONResponse(status_code=400, content={"error": "text must be string"})
        if not isinstance(labels, list) or len(labels) == 0:
            return JSONResponse(status_code=400, content={"error": "labels must be non-empty array"})
        if not all(isinstance(label, str) for label in labels):
            return JSONResponse(status_code=400, content={"error": "all labels must be strings"})
        if not isinstance(threshold, (int, float)):
            return JSONResponse(status_code=400, content={"error": "threshold must be number"})
        if threshold < 0 or threshold > 1:
            return JSONResponse(status_code=400, content={"error": "threshold must be in [0, 1]"})

        # Empty text: return no spans without calling the model
        if text == "":
            return {"spans": []}

        # Call the model
        model_spans = app.state.extractor.predict_entities(text, labels, threshold=threshold)

        # Convert character offsets to byte offsets and build response
        spans = []
        for span in model_spans:
            byte_start, byte_end = char_span_to_byte_span(text, span["start"], span["end"])
            spans.append({
                "text": span["text"],
                "label": span["label"],
                "start": byte_start,
                "end": byte_end,
                "score": span["score"],
            })

        # Sort by start offset
        spans.sort(key=lambda s: s["start"])

        return {"spans": spans}

    return app


if __name__ == "__main__":
    args = parse_args()
    model = resolve_model(args.model)
    uvicorn.run(
        create_app(model=model),
        host=args.host,
        port=args.port,
    )
