# gonnx — upstream divergences & fixes

This file inventories every way our vendored `third_party/gonnx` diverges from
upstream `github.com/advancedclimatesystems/gonnx`, so the fixes can eventually be
contributed back. It is a **living document**: it is deliberately *not* filed upstream
yet — we expect a second model to force further gonnx changes, and the plan is to
package everything into a coherent set of upstream issues/PRs at that later point.
Append new entries here as they are found; do not close the tracking ticket (SOP-83)
until the combined upstream submission is made.

## Provenance

- **Upstream repo:** https://github.com/advancedclimatesystems/gonnx
- **Vendored commit:** `c879ba4` (2025-03-30, "Merge pull request #238 … identity-op"),
  imported as our `dc3b072`. The vendored tree at `dc3b072` was verified
  **byte-identical** to upstream `c879ba4` (251 files, zero diff).
- **Full local divergence:** `git diff dc3b072..HEAD -- third_party/gonnx`
- **Pristine check for any single file:** `git diff dc3b072 -- <path>` empty ⇒ the file
  was pristine upstream when we started (used below to prove each defect is *theirs*).

Every defect below was reproduced on a clean `c879ba4` checkout (go1.26 / arm64) — they
are upstream bugs, not artifacts of our fork.

## The patches — `upstream-patches/`

Each group below carries a ready-to-apply patch under
[`upstream-patches/`](upstream-patches/), generated as `git diff --relative=third_party/gonnx
dc3b072..HEAD -- <group files>`. Paths inside the patches are **upstream-relative**
(`a/ops/...`, not `a/third_party/gonnx/ops/...`), so they apply straight onto a clean
`c879ba4` checkout of `github.com/advancedclimatesystems/gonnx`:

```sh
git clone https://github.com/advancedclimatesystems/gonnx && cd gonnx
git checkout c879ba4
git apply /path/to/upstream-patches/E-sigmoid.patch      # or `git am` after wrapping
```

Each patch includes both the implementation change and its tests. The key implementation
hunk is inlined per group below for a quick read; the linked patch is the full change.
Regenerate all patches after adding a fix with the commands in `upstream-patches/README.md`.

## How to read an entry

Each entry states: the **defect**, a **minimal repro** a maintainer can run in under a
minute, **our in-fork fix** (ticket + PR), the **patch**, and the **recommended upstream
fix**. Pristine confirmation is covered globally by the Provenance section above (`git diff
dc3b072 -- <path>` empty ⇒ the file was pristine upstream) and is called out per-entry only
where a defect shares a file with an earlier fork change. Entries are grouped by theme so
each group can become one upstream issue/PR.

---

## Group A — ONNX `Slice`: gorgonia `Dense.Slice` diverges from ONNX rank/step semantics

Upstream `Slice.Apply` passes the raw ONNX `starts`/`ends`/`steps` straight to gorgonia's
`Dense.Slice` with no ONNX clamping. gorgonia's slicing does not match ONNX in two ways:

- **A1 — negative steps panic.** Any ONNX Slice with `step < 0` (a reverse) panics:
  `d.Slice(tensor.S(4, -1, -1))` → "Invalid slice index. Start: 4, End: -1". Latent in the
  conformance suite (no `test_slice*` case uses a negative step), but Silero's STFT
  reflection padding (`/stft/padding/Slice`, `step=-1`) hits it. *(our fix: SOP-84, PR #47)*
- **A2 — unit axis dropped on positive-step slices.** gorgonia drops an axis sliced to
  length 1; ONNX Slice never changes rank. `(2,3)` sliced `starts=[1,0] ends=[2,2]` returns
  `(2,)` where onnxruntime returns `(1,2)`. Two of upstream's own `TestSlice` cases actually
  *assert* the dropped shape — they encode the bug. *(our fix: SOP-85, PR #48)*

Also handled: gorgonia errors on an empty range `start>=end` where ONNX returns a valid
empty slice.

**Repro:** call `Slice.Apply` with a negative step (panics) and with a length-1 positive
slice (wrong rank).
**Recommended upstream fix:** clamp per the ONNX spec and route all slices through an
explicit gather-by-index (dtype-agnostic `At`/`SetAt`) that preserves rank, empty ranges,
and negative steps — i.e. replace the gorgonia-based positive-step fast path entirely.
**Patch:** [`upstream-patches/A-slice.patch`](upstream-patches/A-slice.patch) (the largest
change — a full `ops/slice` rewrite; read the patch rather than an inline hunk).

## Group B — `IfScalarToSlice` drops `bool` and `uint` scalar dtypes → `Cast` panics

`ops/utils.go` `IfScalarToSlice` normalizes a rank-0 scalar into a 1-element slice but had
no `case bool` and no `case uint8/uint16/uint32/uint64`. gorgonia's `Data()` on a rank-0
scalar returns a bare value, so a scalar of those dtypes falls through `default` unwrapped,
and `ops/convert.go` `ConvertTensorDtype`'s `backing.([]T)` assertion panics
(`interface {} is bool, not []bool` / `is uint8, not []uint8`). Latent — no conformance
case casts a scalar bool/uint; the existing `TestIfScalarToSlice` even asserted `uint8`
stays unwrapped, encoding the bug.

**Repro:** `ConvertTensorDtype(tensor.New(tensor.FromScalar(true)), Float32)` and
`…FromScalar(uint8(5))…` both panic.
**Our fix:** SOP-86 (bool, PR #49) + SOP-91 (uint8/16/32/64, PR #55).
**Recommended upstream fix:** add all five missing `case` arms to `IfScalarToSlice`.
**Patch:** [`upstream-patches/B-scalar-dtypes.patch`](upstream-patches/B-scalar-dtypes.patch)

```diff
--- a/ops/utils.go
+++ b/ops/utils.go
@@ func IfScalarToSlice(value any) any {
 	case int:
 		return []int{data}
+	case uint8:
+		return []uint8{data}
+	case uint16:
+		return []uint16{data}
+	case uint32:
+		return []uint32{data}
+	case uint64:
+		return []uint64{data}
 	case float32:
 		return []float32{data}
@@
 	case complex128:
 		return []complex128{data}
+	case bool:
+		return []bool{data}
 	default:
 		return value
```

## Group C — RNN-family output binding

ONNX RNN/GRU/LSTM outputs are **positional** (`Y`, `Y_h`[, `Y_c`]) and all optional. Two
distinct upstream defects:

- **C1 — LSTM binds outputs by canonical name.** `LSTM.Apply` built an `outputMap` keyed by
  `"Y"/"Y_h"/"Y_c"` then selected by the node's actual output names. A model naming its
  outputs non-canonically (e.g. Silero's `/decoder/rnn/LSTM`) gets `[nil,nil,nil]` with **no
  error** → downstream nil-panic. Upstream's `TestLSTM` masks it by declaring canonical
  names. *(our fix: SOP-87, PR #50 — return positionally, truncated to the declared count)*
- **C2 — GRU/RNN hard-return a fixed 2 outputs.** `GRU.Apply`/`RNN.Apply` always return
  `{Y, Yh}`; the executor's `setOutputTensorsOfNode` errors when `len(names)` differs. A
  GRU/RNN node declaring only `Y` (or only `Y_h`, encoded `["","Y_h"]`) fails with "could
  not set output tensor". GRU/RNN were already positional (no C1 bug) but lacked the
  truncation. *(our fix: SOP-92, PR #57 — capture `n.GetOutput()`, truncate to
  `[:min(len(outputs), 2)]`)*

**Repro:** an LSTM node with non-canonical output names (C1); a GRU node with
`Output: ["Y"]` (C2).
**Recommended upstream fix:** all three ops return outputs positionally, truncated to the
node's declared output count.
**Patches:** [`upstream-patches/C1-lstm-output-binding.patch`](upstream-patches/C1-lstm-output-binding.patch),
[`upstream-patches/C2-gru-rnn-output-count.patch`](upstream-patches/C2-gru-rnn-output-count.patch)

```diff
// C1 — ops/lstm/lstm.go: bind positionally, not by canonical name
-	outputMap := map[string]tensor.Tensor{ ... }        // keyed by "Y"/"Y_h"/"Y_c"
-	for _, outputName := range l.outputs {
-		result = append(result, outputMap[outputName])   // nil on non-canonical names
-	}
+	nOutputs := min(len(l.outputs), 3)
+	return []tensor.Tensor{Y, Yh, Yc}[:nOutputs], nil

// C2 — ops/gru/gru.go & ops/rnn/rnn.go: capture declared outputs, truncate the return
+	// (struct) outputs []string          (constructor) outputs: []string{"Y", "Y_h"}
+	if outputs := n.GetOutput(); len(outputs) > 0 { g.outputs = outputs }   // in Init
-	return []tensor.Tensor{Y, Yh}, nil
+	nOutputs := min(len(g.outputs), 2)
+	return []tensor.Tensor{Y, Yh}[:nOutputs], nil
```

## Group D — `Conv`

- **D1 — `Dense.Slice` drops unit axes inconsistently → 1×1 conv fails to broadcast.** The
  per-kernel loop slices a sub-image and a sub-kernel then multiplies via
  `UnidirectionalBroadcast`. For a 1×1 conv the sub-image collapses to `[C]` while the
  sub-kernel stays `[C,1]`, and the (stricter-than-ONNX) unidirectional broadcast errors.
  Silero's decoder classifier is exactly a 1×1 Conv1D over 128 channels, so `Run` failed at
  `/decoder/decoder/2/Conv`. Latent — every upstream `TestConv` case uses dims > 1. *(our
  fix: SOP-88, PR #51 — restore each sliced sub-tensor to its canonical
  `[nChannels, kernelShape...]` rank before the broadcast)*
- **D2 — `applyConv2D` width loop bounded by padded H, not W.** The inner width loop ran
  `for w := 0; w < paddedX.Shape()[2]` (padded **H**) instead of `Shape()[3]` (padded **W**),
  so any 2D conv wider than it is tall silently left its rightmost output columns zero.
  Tall-narrow inputs were saved by the `dimWOutputIdx >= outputWDim` guard; not hit by Silero
  (1D convs). `X=[1,2,1,3], W=[1,2,1,2], stride 1` → `[37,0]` instead of `[37,47]` (rightmost
  column dropped). *(our fix: SOP-90, PR #54 — one-char bound `[2]`→`[3]`)*

**Repro:** a 1×1 Conv1D (D1); a 2D conv with padded width > padded height (D2).
**Recommended upstream fix:** D1 as above; D2 the one-character bound correction.
**Patch:** [`upstream-patches/D-conv.patch`](upstream-patches/D-conv.patch) (D1 is the larger
`getSubImage`/reshape change; D2 is the one-liner below).

```diff
// D2 — ops/conv/conv.go, applyConv2D inner width loop
-				for w := 0; w < paddedX.Shape()[2]; w += c.strides[1] {   // padded H (bug)
+				for w := 0; w < paddedX.Shape()[3]; w += c.strides[1] {   // padded W (fix)
```

## Group E — `Sigmoid` float32 exp overflow (numerical stability)

`ops/activation.go` `Sigmoid` computed `1/(1+exp(-x))` via gorgonia's `tensor.Exp`, whose
**float32** vectorised kernel does **not** saturate to `+Inf` on overflow: past its max
representable argument (~88.7) it returns NaN/garbage instead (observed: `exp(89)=NaN`,
`exp(90)=-9.3e-39`, `exp(100)=-2.3e-34`). This is the crux for an upstream reader — had it
returned `+Inf` per IEEE 754, `1/(1+Inf)=0` would be *correct*; the defect is the
non-saturating overflow. So `Sigmoid(x)` returned NaN/garbage for `x < -88`
(`Sigmoid([-200,-100,-50,0]) = [-1.6e-10, 1, 1.9e-22, 0.5]`, should be `[~0,~0,~0,0.5]`).
Recurrent ops feed strongly-negative gate pre-activations into `Sigmoid` once their state
magnitude grows, so the NaN/garbage poisons the recurrent state: reproduced as NaN from
frame 48 of a real 10 s telephony clip through Silero VAD, while onnxruntime stays finite on
the identical sequence. This was **also** the cause of the long-standing `TestGru` "0 vs 1"
divergence (a saturated update gate returning 1 instead of ~0) — see Notes. Latent in the
24-frame goldens (never long enough for the state to drift into the overflow regime).

**Repro:** `ops.Sigmoid` on any tensor value `< -88` is non-finite/wrong; a ≥48-frame real
recurrent run NaNs.
**Our fix:** SOP-89 (PR #53) — rewrite to the algebraically-identical, numerically-stable
`0.5*(1+tanh(x/2))`, which exponentiates only through gorgonia's stable `tensor.Tanh`.
**Recommended upstream fix:** the same stable reformulation (or clamp the exp argument).
**Patch:** [`upstream-patches/E-sigmoid.patch`](upstream-patches/E-sigmoid.patch)

```diff
--- a/ops/activation.go
+++ b/ops/activation.go
 func Sigmoid(X tensor.Tensor) (tensor.Tensor, error) {
-	negX, err := tensor.Neg(X)
-	if err != nil { return nil, err }
-	expX, err := tensor.Exp(negX)
-	if err != nil { return nil, err }
-	typedOne, err := GetValueAsTensorType(1.0, expX.Dtype())
-	if err != nil { return nil, err }
-	numeratorX, err := tensor.Add(typedOne, expX)
-	if err != nil { return nil, err }
-	return tensor.Div(typedOne, numeratorX)
+	typedHalf, err := GetValueAsTensorType(0.5, X.Dtype())
+	if err != nil { return nil, err }
+	halfX, err := tensor.Mul(X, typedHalf)
+	if err != nil { return nil, err }
+	tanhHalfX, err := tensor.Tanh(halfX)
+	if err != nil { return nil, err }
+	typedOne, err := GetValueAsTensorType(1.0, X.Dtype())
+	if err != nil { return nil, err }
+	onePlusTanh, err := tensor.Add(tanhHalfX, typedOne)
+	if err != nil { return nil, err }
+	return tensor.Mul(onePlusTanh, typedHalf)
 }
```

---

## Test-infrastructure & tooling defects (upstream, lower priority)

These are upstream bugs in the test/build harness rather than operator math. Worth bundling
into the same contribution.

- **T1 — `make test_data` is a permanent no-op once `test_data/` exists.** `.PHONY` omits
  `test_data`, and the target name collides with the directory the recipe creates; after the
  first run, make reports "`test_data' is up to date" forever. Fix: add `test_data` to
  `.PHONY` (one line). *(not yet fixed in-fork)*
- **T2 — `TestOps` panics on any rank-0 (scalar) operator output.** `ops_test.go` compares
  with `assert.InDeltaSlice`, which rejects non-slices; a rank-0 tensor's `Data()` is a bare
  value. Dormant upstream (no scalar-returning op registered) — fires the moment one is added
  (we hit it adding `Size`). Fix: branch on `expectedTensor.IsScalar()` → `assert.InDelta`.
- **T3 — float assertions non-portable across architectures (arm64).** `linearregressor`,
  `logsoftmax`, `softmax` (and formerly `gru`, `lstm`) compare float32 outputs with exact
  `assert.Equal` against values baked on another platform; the diffs are last-ULP
  (`softmax` expects `0.087144315`, gets `0.08714432`). The math is right; the assertions are
  not portable. Fix: `assert.InDeltaSlice(..., 1e-3)`. *(ours: SOP-75; `gru`/`lstm` already
  cleared as a side effect of the Group E fix's tolerance conversions.)* **Patch:**
  [`upstream-patches/T3-float-tolerances.patch`](upstream-patches/T3-float-tolerances.patch)
  (test-only; the `gru`/`lstm` tolerance changes ride along in the C1/C2 patches).
- **T4 — `expectedTests` is a hardcoded name list.** Registering any operator breaks
  `assert.Equal(expectedTests, runnedTests)`. Brittle, but arguably intentional (explicit
  about which tests run). **Mention, do not fix** — a design note, not a bug report.

## Features worth offering upstream

- **`ops/size`** — the ONNX `Size` operator (SOP-67), with conformance coverage.
- **`ops/pad`** — the ONNX `Pad` operator, constant/reflect/edge (SOP-74). Verified
  bit-identical to onnxruntime across 690 cases covering every `(n, begin, end, mode)` for
  n ≤ 5. Fixes two subtleties most implementations get wrong: reflect/edge bounds must be
  against the axis size that *survives* the opposite side's crop, and cropping is
  mode-independent.

## Notes / status corrections (2026-07-11)

- The original SOP-83 ticket listed "gru produces 0 where the test expects 1" (item D) as an
  **unknown-severity, undiagnosed** defect. It is now **diagnosed**: it was Group E (the
  Sigmoid float32-exp overflow), confirmed by reverting the Sigmoid (divergence returns) and
  by an onnxruntime cross-check (Y_h last element is 0, never 1). Not a separate GRU bug.
- The arm64 test failures (T3) are down from 5 packages to 3 (`gru`/`lstm` cleared by the
  Group E tolerance conversions); `linearregressor`/`logsoftmax`/`softmax` remain for SOP-75.

## Adding new entries

When the next model forces further gonnx changes: append a new Group (or extend an existing
one), keep the defect/pristine/repro/fix/recommendation shape, run `git diff dc3b072 -- <path>`
to confirm the pristine baseline, add a patch to `upstream-patches/` and reference it, and
update the status notes. Regenerate all patches with `upstream-patches/README.md`'s commands
after any fork change so they stay in sync. Keep SOP-83 open until the combined upstream
submission is actually made.
