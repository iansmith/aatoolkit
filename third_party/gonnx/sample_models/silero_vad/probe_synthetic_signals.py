"""Evidence for why goldens.json uses a real-speech fixture instead of a synthetic tone.

Silero is trained to reject non-speech and is good at it. This script scores a handful
of synthetic candidates against the vendored model and prints their peak probability.
The table in PROVENANCE.md is this script's output; re-run it to check the numbers.

    pip install -r requirements.txt
    python probe_synthetic_signals.py
"""

import pathlib

import numpy as np
import onnxruntime as ort

HERE = pathlib.Path(__file__).parent
MODEL = HERE.parent / "onnx_models" / "silero_vad.onnx"

SAMPLE_RATE = 8000
WINDOW = 256
FRAMES = 10
SEED = 7


def make(kind: str, rng: np.random.Generator, i: int) -> np.ndarray:
    t = (np.arange(WINDOW) + i * WINDOW) / SAMPLE_RATE

    if kind == "near-silence (sigma=0.001 noise)":
        x = rng.standard_normal(WINDOW) * 0.001

    elif kind == "white noise (sigma=0.3)":
        x = rng.standard_normal(WINDOW) * 0.3

    elif kind == "AM-modulated noise (4 Hz)":
        env = 0.5 * (1 + np.sin(2 * np.pi * 4 * t))
        x = rng.standard_normal(WINDOW) * 0.4 * env

    elif kind == "harmonic stack 140/280/700 Hz":
        x = 0.6 * (
            0.50 * np.sin(2 * np.pi * 140 * t)
            + 0.25 * np.sin(2 * np.pi * 280 * t)
            + 0.15 * np.sin(2 * np.pi * 700 * t)
        )

    elif kind == "glottal pulse train -> formants":
        f0 = 120 + 20 * np.sin(2 * np.pi * 3 * t)
        phase = np.cumsum(2 * np.pi * f0 / SAMPLE_RATE)
        x = np.where(np.mod(phase, 2 * np.pi) < 0.35, 1.0, -0.1)
        for centre, bandwidth in ((700, 110), (1220, 130)):
            r = np.exp(-np.pi * bandwidth / SAMPLE_RATE)
            theta = 2 * np.pi * centre / SAMPLE_RATE
            y = np.zeros_like(x)
            y1 = y2 = 0.0
            for k in range(len(x)):
                v = x[k] + 2 * r * np.cos(theta) * y1 - r * r * y2
                y[k] = v
                y2, y1 = y1, v
            x = y / (np.max(np.abs(y)) + 1e-9)
        x = 0.7 * x + rng.standard_normal(WINDOW) * 0.02

    elif kind == "chirp 300->900 Hz":
        # Instantaneous frequency ramps 300 -> 900 Hz every 250 ms, i.e.
        # f(tau) = 300 + 2400*tau for tau in [0, 0.25).
        #
        # Phase is the integral of f, not f(t)*t. Using f(t)*t would sweep to 1452 Hz
        # and inject a phase discontinuity at every sawtooth wrap. Integrating in
        # closed form (rather than cumsum) keeps the phase continuous across frame
        # boundaries, since each frame is generated independently from absolute t.
        period, ramp = 0.25, 2400.0
        cycles, tau = np.divmod(t, period)
        phase_per_cycle = 300 * period + 0.5 * ramp * period**2
        phase = 2 * np.pi * (cycles * phase_per_cycle + 300 * tau + 0.5 * ramp * tau**2)
        x = 0.6 * np.sin(phase)

    else:
        raise ValueError(kind)

    return x.astype(np.float32)


def main() -> None:
    opts = ort.SessionOptions()
    opts.graph_optimization_level = ort.GraphOptimizationLevel.ORT_DISABLE_ALL
    session = ort.InferenceSession(str(MODEL), opts, providers=["CPUExecutionProvider"])
    sr = np.array(SAMPLE_RATE, dtype=np.int64)

    kinds = [
        "near-silence (sigma=0.001 noise)",
        "white noise (sigma=0.3)",
        "AM-modulated noise (4 Hz)",
        "harmonic stack 140/280/700 Hz",
        "glottal pulse train -> formants",
        "chirp 300->900 Hz",
    ]

    print(f"{'signal':34s} {'peak':>7s} {'mean':>7s}")
    for kind in kinds:
        rng = np.random.default_rng(SEED)
        state = np.zeros((2, 1, 128), dtype=np.float32)
        probs = []
        for i in range(FRAMES):
            frame = make(kind, rng, i)
            out, state = session.run(None, {"input": frame[None, :], "state": state, "sr": sr})
            probs.append(float(out[0][0]))
        print(f"{kind:34s} {max(probs):7.3f} {float(np.mean(probs)):7.3f}")

    print(
        "\nThe committed speech fixture (speech_8k.raw) scores 0.94-0.97 on the same model."
        "\nThese synthetic signals are underspecified in the abstract: other harmonic stacks"
        "\nDO cross 0.5. The point is not that no synthetic signal can read as speech, but"
        "\nthat the obvious ones here do not, so goldens built from them would exercise only"
        "\none end of the model's output range."
    )


if __name__ == "__main__":
    main()
