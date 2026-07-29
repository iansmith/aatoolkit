"""
Pure unit tests for steering evaluation (no model, no network).

Tests the judge_case function logic and the gold set structure,
independent of GLiNER model loading.
"""

import json
import subprocess
import sys
import tempfile
from pathlib import Path


def load_gold_set():
    """Load the gold set from scripts/testdata/steering_cases.json."""
    repo_root = Path(__file__).resolve().parent.parent
    cases_path = repo_root / "scripts" / "testdata" / "steering_cases.json"
    with open(cases_path) as f:
        return json.load(f)


def test_judge_case_passes_on_full_contrast():
    """Test judge_case passes when steered row, control row, and offsets all match."""
    from eval_steering import judge_case

    case = {
        "id": "test",
        "category": "direct",
        "text": "Bill's birthday is July 5th",
        "steered_labels": ["birthday_person", "birthday_date"],
        "control_labels": ["person", "date"],
        "threshold": 0.5,
        "expect_steered": [
            {"label": "birthday_person", "text": "Bill"},
            {"label": "birthday_date", "text": "July 5th"}
        ],
        "expect_control": [
            {"label": "person", "text": "Bill"},
            {"label": "date", "text": "July 5th"}
        ]
    }

    steered_spans = [
        {"label": "birthday_person", "text": "Bill", "start": 0, "end": 4, "score": 0.9},
        {"label": "birthday_date", "text": "July 5th", "start": 21, "end": 29, "score": 0.85}
    ]

    control_spans = [
        {"label": "person", "text": "Bill", "start": 0, "end": 4, "score": 0.9},
        {"label": "date", "text": "July 5th", "start": 21, "end": 29, "score": 0.85}
    ]

    passed, failures = judge_case(case, steered_spans, control_spans)
    assert passed is True, f"Expected pass but got failures: {failures}"
    assert failures == []


def test_judge_case_fails_when_steered_label_missing():
    """Test judge_case fails when steered row is missing expected label."""
    from eval_steering import judge_case

    case = {
        "id": "test",
        "category": "direct",
        "text": "Bill's birthday is July 5th",
        "steered_labels": ["birthday_person", "birthday_date"],
        "control_labels": ["person", "date"],
        "threshold": 0.5,
        "expect_steered": [
            {"label": "birthday_person", "text": "Bill"},
            {"label": "birthday_date", "text": "July 5th"}
        ],
        "expect_control": [
            {"label": "person", "text": "Bill"},
            {"label": "date", "text": "July 5th"}
        ]
    }

    steered_spans = [
        {"label": "birthday_person", "text": "Bill", "start": 0, "end": 4, "score": 0.9}
    ]

    control_spans = [
        {"label": "person", "text": "Bill", "start": 0, "end": 4, "score": 0.9},
        {"label": "date", "text": "July 5th", "start": 21, "end": 29, "score": 0.85}
    ]

    passed, failures = judge_case(case, steered_spans, control_spans)
    assert passed is False
    assert any("birthday_date" in f for f in failures), f"Expected failure mentioning 'birthday_date', got {failures}"


def test_judge_case_fails_when_span_offsets_differ():
    """Test judge_case fails when steered and control rows have different span offsets.

    This is the critical test proving the control row does work.
    """
    from eval_steering import judge_case

    case = {
        "id": "test",
        "category": "direct",
        "text": "Bill's birthday is July 5th",
        "steered_labels": ["birthday_person", "birthday_date"],
        "control_labels": ["person", "date"],
        "threshold": 0.5,
        "expect_steered": [
            {"label": "birthday_person", "text": "Bill"},
            {"label": "birthday_date", "text": "July 5th"}
        ],
        "expect_control": [
            {"label": "person", "text": "Bill"},
            {"label": "date", "text": "July 5th"}
        ]
    }

    steered_spans = [
        {"label": "birthday_person", "text": "Bill", "start": 0, "end": 4, "score": 0.9},
        {"label": "birthday_date", "text": "July 5th", "start": 21, "end": 29, "score": 0.85}
    ]

    control_spans = [
        {"label": "person", "text": "Bill", "start": 0, "end": 5, "score": 0.9},
        {"label": "date", "text": "July 5th", "start": 21, "end": 29, "score": 0.85}
    ]

    passed, failures = judge_case(case, steered_spans, control_spans)
    assert passed is False
    assert any("offset" in f.lower() for f in failures), f"Expected failure mentioning offset mismatch, got {failures}"


def test_judge_case_no_fact_requires_both_rows_empty():
    """Test judge_case enforces both rows empty for no_fact category."""
    from eval_steering import judge_case

    case = {
        "id": "test",
        "category": "no_fact",
        "text": "Yeah, that sounds fine, thanks",
        "steered_labels": ["person", "date"],
        "control_labels": ["entity", "event"],
        "threshold": 0.5,
        "expect_steered": [],
        "expect_control": []
    }

    steered_spans = []
    control_spans = [
        {"label": "entity", "text": "I", "start": 0, "end": 1, "score": 0.8}
    ]

    passed, failures = judge_case(case, steered_spans, control_spans)
    assert passed is False
    assert len(failures) > 0, "Expected failures for no_fact case with non-empty control row"

    steered_spans = []
    control_spans = []
    passed, failures = judge_case(case, steered_spans, control_spans)
    assert passed is True


def test_gold_set_is_well_formed():
    """Test the gold set has correct shape and category distribution."""
    cases = load_gold_set()

    # At least 12 cases total
    assert len(cases) >= 12, f"Gold set has {len(cases)} cases, expected at least 12"

    # Count by category
    category_counts = {}
    for case in cases:
        cat = case.get("category")
        category_counts[cat] = category_counts.get(cat, 0) + 1

    assert category_counts.get("elliptical", 0) >= 4, f"Expected >=4 elliptical, got {category_counts.get('elliptical', 0)}"
    assert category_counts.get("direct", 0) >= 2, f"Expected >=2 direct, got {category_counts.get('direct', 0)}"
    assert category_counts.get("no_fact", 0) >= 2, f"Expected >=2 no_fact, got {category_counts.get('no_fact', 0)}"
    assert category_counts.get("multi_entity", 0) >= 2, f"Expected >=2 multi_entity, got {category_counts.get('multi_entity', 0)}"

    # Validate each case
    for case in cases:
        # Required fields
        assert "id" in case, f"Case missing 'id'"
        assert "category" in case, f"Case {case.get('id')} missing 'category'"
        assert case["category"] in ["elliptical", "direct", "no_fact", "multi_entity"], \
            f"Case {case['id']} has invalid category: {case['category']}"

        # No duplicate 'kind' field
        assert "kind" not in case, f"Case {case['id']} should not have 'kind' field"

        # Non-empty label lists
        assert "steered_labels" in case and case["steered_labels"], f"Case {case['id']} missing or empty steered_labels"
        assert "control_labels" in case and case["control_labels"], f"Case {case['id']} missing or empty control_labels"

        # Threshold in range
        assert "threshold" in case and 0 <= case["threshold"] <= 1, \
            f"Case {case['id']} has invalid threshold: {case.get('threshold')}"

        # Disjoint labels for non-no_fact cases
        if case["category"] != "no_fact":
            steered_set = set(case["steered_labels"])
            control_set = set(case["control_labels"])
            assert len(steered_set & control_set) == 0, \
                f"Case {case['id']} ({case['category']}) has overlapping label lists"

        # Empty expect arrays for no_fact
        if case["category"] == "no_fact":
            assert case.get("expect_steered") == [], f"Case {case['id']} (no_fact) should have empty expect_steered"
            assert case.get("expect_control") == [], f"Case {case['id']} (no_fact) should have empty expect_control"

        # Expect array text is substring of case text
        for expect_list in [case.get("expect_steered", []), case.get("expect_control", [])]:
            for item in expect_list:
                assert item["text"] in case["text"], \
                    f"Case {case['id']}: expected text '{item['text']}' not in case text"


def test_entrypoint_missing_cases_from_other_cwd():
    """Test runner exits 2 with repo-root-resolved path when cases file missing.

    This verifies the path resolution happens before model load.
    """
    repo_root = Path(__file__).resolve().parent.parent
    eval_script = repo_root / "scripts" / "eval_steering.py"

    with tempfile.TemporaryDirectory() as tmpdir:
        result = subprocess.run(
            [sys.executable, str(eval_script), "--cases", "scripts/testdata/definitely-missing.json"],
            cwd=tmpdir,
            capture_output=True,
            text=True,
            timeout=60
        )

        assert result.returncode == 2, f"Expected exit code 2, got {result.returncode}"
        expected_path = str(repo_root / "scripts" / "testdata" / "definitely-missing.json")
        assert expected_path in result.stderr, \
            f"Expected absolute path {expected_path} in stderr, got: {result.stderr}"
