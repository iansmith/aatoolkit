"""
Test suite for facts_server.py extraction sidecar.

The handler tests drive create_app(warmup=False, extractor=<stub>) through
fastapi.testclient.TestClient, with a stub extractor that records calls and
returns scripted character-offset spans. The remaining tests call functions
or the entrypoint directly.

This suite runs in an environment with fastapi/uvicorn but NOT gliner/torch/transformers.
"""

import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest
from fastapi.testclient import TestClient


# Fixture to load the wire contract from extract/testdata/extract_wire_fixture.json
@pytest.fixture
def wire_fixture():
    fixture_path = Path(__file__).resolve().parent.parent / "extract" / "testdata" / "extract_wire_fixture.json"
    with open(fixture_path) as f:
        return json.load(f)


# Stub extractor for handler tests
class StubExtractor:
    """A stub GLiNER model that records calls and returns scripted spans."""

    def __init__(self, spans_to_return=None):
        self.calls = []
        self.spans_to_return = spans_to_return or []

    def predict_entities(self, text, labels, threshold=None):
        """Record the call and return scripted spans (as character offsets)."""
        self.calls.append({
            "text": text,
            "labels": labels,
            "threshold": threshold,
        })
        return self.spans_to_return


# ============================================================================
# Handler tests (POST /extract and GET /healthz via TestClient)
# ============================================================================

def test_wire_fixture_request_reaches_extractor(wire_fixture):
    """POST the fixture's request_json; stub records exactly one call with correct args."""
    import facts_server

    stub = StubExtractor(spans_to_return=[])
    app = facts_server.create_app(warmup=False, extractor=stub)
    client = TestClient(app)

    response = client.post("/extract", content=wire_fixture["request_json"], headers={"content-type": "application/json"})

    assert len(stub.calls) == 1
    assert stub.calls[0]["text"] == "Bill on the 5th of July"
    assert stub.calls[0]["labels"] == ["birthday_person", "birthday_date"]
    assert stub.calls[0]["threshold"] == 0.5


def test_wire_fixture_response_shape(wire_fixture):
    """With stub returning fixture's spans as character offsets, response equals fixture."""
    import facts_server

    # Fixture spans are already in the right format for this ASCII text
    fixture_spans = wire_fixture["response"]["spans"]

    stub = StubExtractor(spans_to_return=fixture_spans)
    app = facts_server.create_app(warmup=False, extractor=stub)
    client = TestClient(app)

    response = client.post("/extract", json=wire_fixture["request"])

    assert response.status_code == 200
    assert response.json() == wire_fixture["response"]


def test_char_span_to_byte_span(wire_fixture):
    """char_span_to_byte_span returns exact pairs from byte_offset_cases."""
    import facts_server

    for case in wire_fixture["byte_offset_cases"]:
        byte_start, byte_end = facts_server.char_span_to_byte_span(
            case["text"], case["char_start"], case["char_end"]
        )
        assert (byte_start, byte_end) == (case["byte_start"], case["byte_end"])


def test_rejects_malformed_requests():
    """Parametrized over 6 malformed request bodies; each returns 400 without model call."""
    import facts_server

    stub = StubExtractor()
    app = facts_server.create_app(warmup=False, extractor=stub)
    client = TestClient(app)

    malformed_requests = [
        {},  # missing text
        {"text": "foo"},  # missing labels
        {"text": "foo", "labels": []},  # empty labels
        {"text": "foo", "labels": ["ok", 7]},  # labels contains non-string
        {"text": "foo", "labels": ["ok"]},  # missing threshold
        {"text": "foo", "labels": ["ok"], "threshold": 1.5},  # threshold out of range
    ]

    for req in malformed_requests:
        stub.calls.clear()
        response = client.post("/extract", json=req)
        assert response.status_code == 400, f"Expected 400 for request {req}, got {response.status_code}"
        assert len(stub.calls) == 0, f"Model should not be called for malformed request {req}"


def test_empty_text_returns_no_spans_without_model_call():
    """text=\"\" returns 200 {\"spans\": []} without calling the model."""
    import facts_server

    stub = StubExtractor()
    app = facts_server.create_app(warmup=False, extractor=stub)
    client = TestClient(app)

    response = client.post("/extract", json={"text": "", "labels": ["x"], "threshold": 0.5})

    assert response.status_code == 200
    assert response.json() == {"spans": []}
    assert len(stub.calls) == 0


def test_spans_sorted_by_start():
    """Stub returns spans in reverse start order; response lists them in ascending start order."""
    import facts_server

    unsorted_spans = [
        {"text": "later", "label": "x", "start": 12, "end": 17, "score": 0.9},
        {"text": "first", "label": "y", "start": 0, "end": 5, "score": 0.8},
    ]

    stub = StubExtractor(spans_to_return=unsorted_spans)
    app = facts_server.create_app(warmup=False, extractor=stub)
    client = TestClient(app)

    response = client.post("/extract", json={"text": "first and later", "labels": ["x", "y"], "threshold": 0.5})

    assert response.status_code == 200
    result = response.json()
    assert result["spans"][0]["start"] == 0
    assert result["spans"][1]["start"] == 12


def test_healthz():
    """GET /healthz returns 200 {\"status\": \"ok\"}."""
    import facts_server

    stub = StubExtractor()
    app = facts_server.create_app(warmup=False, extractor=stub)
    client = TestClient(app)

    response = client.get("/healthz")

    assert response.status_code == 200
    assert response.json() == {"status": "ok"}


# ============================================================================
# Argument parsing tests
# ============================================================================

def test_parse_args_defaults():
    """parse_args([]) yields defaults: host=\"127.0.0.1\", port=7791, model=\"urchade/gliner_small-v2.1\"."""
    import facts_server

    args = facts_server.parse_args([])

    assert args.host == "127.0.0.1"
    assert args.port == 7791
    assert args.model == "urchade/gliner_small-v2.1"


# ============================================================================
# Model argument handling
# ============================================================================

def test_model_argument_reaches_loader():
    """Monkeypatch load_model; verify create_app calls it exactly once with parsed flag."""
    import facts_server

    recorded_call = []

    def mock_load_model(model_id):
        recorded_call.append(model_id)
        return StubExtractor()

    with patch.object(facts_server, "load_model", side_effect=mock_load_model):
        app = facts_server.create_app(model="X/Y")
        # Trigger the lifespan to run
        with TestClient(app):
            pass

    assert recorded_call == ["X/Y"]


# ============================================================================
# Model path resolution
# ============================================================================

def test_resolve_model(tmp_path):
    """Table test for model path resolution with exact inputs and expected values."""
    import facts_server

    repo_root = Path(__file__).resolve().parent.parent

    test_cases = [
        # (input, expected_result or expected_exception)
        ("urchade/gliner_small-v2.1", "urchade/gliner_small-v2.1"),  # repo id with /
        ("models/gliner", "models/gliner"),  # repo id without special prefix
        ("./scripts", str(repo_root / "scripts")),  # relative path, normalized
        ("~", str(Path.home())),  # home expansion
        (str(tmp_path), str(tmp_path)),  # absolute existing directory unchanged
    ]

    for input_val, expected in test_cases:
        result = facts_server.resolve_model(input_val)
        assert result == expected, f"resolve_model({input_val!r}) = {result!r}, expected {expected!r}"

    # Test that non-existent path raises SystemExit with code 2
    with pytest.raises(SystemExit) as exc_info:
        facts_server.resolve_model("./no-such-model-dir")
    assert exc_info.value.code == 2


# ============================================================================
# Environment setup
# ============================================================================

def test_offline_env_is_set_before_gliner_import():
    """After importing facts_server, HF_HUB_OFFLINE and TRANSFORMERS_OFFLINE are set."""
    import facts_server

    assert os.environ.get("HF_HUB_OFFLINE") == "1"
    assert os.environ.get("TRANSFORMERS_OFFLINE") == "1"


# ============================================================================
# Entrypoint tests (subprocess, from foreign cwd)
# ============================================================================

def test_entrypoint_missing_model_from_other_cwd():
    """Run from foreign cwd with --model ./no-such-model-dir; exits 2 with repo-root-resolved path in stderr."""
    script_path = Path(__file__).resolve().parent / "facts_server.py"
    repo_root = Path(__file__).resolve().parent.parent

    with tempfile.TemporaryDirectory() as tmpdir:
        result = subprocess.run(
            [sys.executable, str(script_path), "--model", "./no-such-model-dir"],
            cwd=tmpdir,
            capture_output=True,
            text=True,
            timeout=60,
        )

        assert result.returncode == 2
        expected_path = str(repo_root / "no-such-model-dir")
        assert expected_path in result.stderr


def test_entrypoint_help_from_other_cwd():
    """Run --help from foreign cwd; exits 0 with help text including port 7791 and default model."""
    script_path = Path(__file__).resolve().parent / "facts_server.py"

    with tempfile.TemporaryDirectory() as tmpdir:
        result = subprocess.run(
            [sys.executable, str(script_path), "--help"],
            cwd=tmpdir,
            capture_output=True,
            text=True,
            timeout=60,
        )

        assert result.returncode == 0
        assert "--model" in result.stdout
        assert "--host" in result.stdout
        assert "--port" in result.stdout
        assert "7791" in result.stdout
        assert "urchade/gliner_small-v2.1" in result.stdout
