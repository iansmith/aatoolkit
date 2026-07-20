# upstream-patches

Ready-to-apply patches for each upstream-gonnx divergence catalogued in
[`../UPSTREAM_DIVERGENCES.md`](../UPSTREAM_DIVERGENCES.md). Each patch is the diff of our
fork against the pristine vendored commit `dc3b072` (= upstream `c879ba4`), with
**upstream-relative paths** (`a/ops/...`), so it applies directly onto a clean upstream
checkout:

```sh
git clone https://github.com/advancedclimatesystems/gonnx && cd gonnx
git checkout c879ba4
git apply /abs/path/to/upstream-patches/<group>.patch
```

Each patch bundles the implementation change **and** its tests for that group.

| Patch | Group (see UPSTREAM_DIVERGENCES.md) |
|---|---|
| `A-slice.patch` | A — Slice rank/step semantics |
| `B-scalar-dtypes.patch` | B — `IfScalarToSlice` bool + uint scalar dtypes |
| `C1-lstm-output-binding.patch` | C1 — LSTM positional output binding |
| `C2-gru-rnn-output-count.patch` | C2 — GRU/RNN output-count truncation |
| `D-conv.patch` | D — Conv 1×1 unit-axis + 2D width-loop |
| `E-sigmoid.patch` | E — Sigmoid float32-exp overflow |
| `T3-float-tolerances.patch` | T3 — non-portable float assertions (test-only) |

## Regenerating (run from the monorepo root)

These are the exact commands that produced the patches; re-run them after any change to the
fork so the patches stay in sync with the code. `--relative=third_party/gonnx` rewrites the
paths to be upstream-root-relative.

```sh
P=third_party/gonnx; R="--relative=$P"
git diff $R dc3b072..HEAD -- $P/ops/slice/                                             > $P/upstream-patches/A-slice.patch
git diff $R dc3b072..HEAD -- $P/ops/utils.go $P/ops/utils_test.go $P/ops/convert_test.go > $P/upstream-patches/B-scalar-dtypes.patch
git diff $R dc3b072..HEAD -- $P/ops/lstm/                                              > $P/upstream-patches/C1-lstm-output-binding.patch
git diff $R dc3b072..HEAD -- $P/ops/gru/ $P/ops/rnn/                                   > $P/upstream-patches/C2-gru-rnn-output-count.patch
git diff $R dc3b072..HEAD -- $P/ops/conv/                                              > $P/upstream-patches/D-conv.patch
git diff $R dc3b072..HEAD -- $P/ops/activation.go $P/ops/activation_test.go            > $P/upstream-patches/E-sigmoid.patch
git diff $R dc3b072..HEAD -- $P/ops/linearregressor/ $P/ops/logsoftmax/ $P/ops/softmax/ > $P/upstream-patches/T3-float-tolerances.patch
```

> Not yet filed upstream — deliberately. See UPSTREAM_DIVERGENCES.md: a second model is
> expected to force further gonnx changes, and everything will be packaged into one
> coherent upstream submission at that point (tracking ticket SOP-83, kept open until then).
