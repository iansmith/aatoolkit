#!/usr/bin/env python3
"""
Steering evaluation runner: contrasts steered vs unsteered label sets over a gold set.

This is roughly a dozen hand-written examples testing one claim: label-set steering
recovers a predicate the user's turn does not itself contain. It is NOT a precision/recall
figure, NOT a characterization of accuracy, and NOT an implementation-equivalence oracle.
Do not cite this as grounds for an ONNX port or other broader validation claims.

Each steering case runs twice over the same turn: once with steered labels, once with
control labels. The contrast assertions check:
1. Steered row returns expected (label, text) pairs
2. Control row returns expected (label, text) pairs
3. Span offsets are identical across both rows (proves steering came from label choice)
4. For no_fact cases: both rows return zero spans

This runner deliberately is NOT named test_*.py so pytest's collection never picks it up.
It is a standalone program: exits 0 when all cases pass, 1 when any fails, 2 on
configuration errors (missing cases file, before model load).
"""

import argparse
import json
import os
import sys
from pathlib import Path

from fastapi.testclient import TestClient

import facts_server


def _pairs(spans: list) -> set:
    return {(s["label"], s["text"]) for s in spans}


def _offsets(spans: list) -> set:
    return {(s["start"], s["end"]) for s in spans}


def _check_row_pairs(actual_spans: list, expected: list, row_name: str) -> list[str]:
    actual_pairs = _pairs(actual_spans)
    expected_pairs = _pairs(expected)
    if actual_pairs != expected_pairs:
        return [f"{row_name} row mismatch: expected {expected_pairs}, got {actual_pairs}"]
    return []


def _check_offsets(steered_spans: list, control_spans: list) -> list[str]:
    steered_offsets = _offsets(steered_spans)
    control_offsets = _offsets(control_spans)
    if steered_offsets != control_offsets:
        return [f"Span offset mismatch: steered {steered_offsets} != control {control_offsets}"]
    return []


def _check_no_fact(category: str, steered_spans: list, control_spans: list) -> list[str]:
    if category == "no_fact" and (steered_spans or control_spans):
        return [f"no_fact case must return zero spans in both rows, got steered={len(steered_spans)}, control={len(control_spans)}"]
    return []


def judge_case(case: dict, steered_spans: list, control_spans: list) -> tuple[bool, list[str]]:
    """
    Judge a steering case against expected outputs.

    Returns (passed: bool, failures: list[str]).
    Implements the three main assertions plus the no_fact special case.
    """
    failures = []
    failures += _check_row_pairs(steered_spans, case.get("expect_steered", []), "Steered")
    failures += _check_row_pairs(control_spans, case.get("expect_control", []), "Control")
    failures += _check_offsets(steered_spans, control_spans)
    failures += _check_no_fact(case.get("category"), steered_spans, control_spans)

    return (len(failures) == 0, failures)


def run_case(client, case: dict) -> dict:
    """Run one steering case's steered and control rows and judge the result."""
    resp = client.post("/extract", json={
        "text": case["text"],
        "labels": case["steered_labels"],
        "threshold": case["threshold"]
    })
    steered_spans = resp.json()["spans"] if resp.status_code == 200 else []

    resp = client.post("/extract", json={
        "text": case["text"],
        "labels": case["control_labels"],
        "threshold": case["threshold"]
    })
    control_spans = resp.json()["spans"] if resp.status_code == 200 else []

    passed, failures = judge_case(case, steered_spans, control_spans)

    return {
        "id": case.get("id", "unknown"),
        "category": case.get("category", "unknown"),
        "status": "PASS" if passed else "FAIL",
        "steered_spans": steered_spans,
        "control_spans": control_spans,
        "failures": failures
    }


def print_report(results: list[dict]) -> None:
    """Print the per-case table and the summary footer."""
    print("\n" + "="*120)
    print(f"{'Case ID':<25} {'Category':<15} {'Status':<8} {'Steered':<20} {'Control':<20} {'Details':<20}")
    print("="*120)

    for result in results:
        steered_count = len(result["steered_spans"])
        control_count = len(result["control_spans"])
        failures_text = result["failures"][0][:15] if result["failures"] else ""

        print(f"{result['id']:<25} {result['category']:<15} {result['status']:<8} "
              f"{steered_count:<20} {control_count:<20} {failures_text:<20}")

        for failure in result["failures"]:
            print(f"  └─ {failure}")

    print("="*120)

    passed_count = sum(1 for r in results if r["status"] == "PASS")
    print(f"\nSummary: {passed_count}/{len(results)} cases passed")
    print(f"This evaluation is ~{len(results)} hand-written examples testing one claim.")
    print(f"It is NOT a precision/recall figure, NOT a characterization of accuracy, "
          f"and NOT an implementation-equivalence oracle.")
    print()


def resolve_cases_path(cases_arg: str) -> Path:
    """
    Resolve a cases file path against the repository root.

    If the path is relative, resolve it against the repository root (parent of scripts/).
    If absolute, use it as-is. If the file doesn't exist, exit 2 with the resolved path.
    """
    if cases_arg.startswith("/"):
        resolved = Path(cases_arg)
    else:
        repo_root = Path(__file__).resolve().parent.parent
        resolved = repo_root / cases_arg

    if not resolved.exists():
        print(f"eval_steering: cases file not found: {resolved.resolve()}", file=sys.stderr)
        sys.exit(2)

    return resolved


def main():
    parser = argparse.ArgumentParser(
        description="Steering evaluation runner",
        formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument(
        "--cases",
        default="scripts/testdata/steering_cases.json",
        help="Path to gold set JSON (default: scripts/testdata/steering_cases.json)"
    )
    parser.add_argument(
        "--model",
        default=os.environ.get("AATOOLKIT_FACTS_MODEL", "urchade/gliner_small-v2.1"),
        help="Model identifier or path (default: env AATOOLKIT_FACTS_MODEL or urchade/gliner_small-v2.1)"
    )

    args = parser.parse_args()

    # Resolve cases path (exits 2 if missing, before model load)
    cases_path = resolve_cases_path(args.cases)

    # Load gold set
    with open(cases_path) as f:
        cases = json.load(f)

    model = facts_server.resolve_model(args.model)

    with TestClient(facts_server.create_app(model=model)) as client:
        results = [run_case(client, case) for case in cases]

    print_report(results)

    all_passed = all(r["status"] == "PASS" for r in results)
    sys.exit(0 if all_passed else 1)


if __name__ == "__main__":
    main()
