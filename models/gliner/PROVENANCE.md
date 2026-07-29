# GLiNER — model provenance

| Field | Value |
|-------|-------|
| Upstream repo | https://huggingface.co/urchade/gliner_small-v2.1 |
| Pinned revision | 4e091416cf7c3481db542c2a3d26156916f3a47f |
| License | apache-2.0 — https://huggingface.co/urchade/gliner_small-v2.1 |
| Base encoder | microsoft/deberta-v3-small at a36c739020e01763fe789b4b85e2df55d6180012 |
| Sha256 (pytorch_model.bin) | 1d4e83e4e4ae4ae0a4fbc81a32ee6de480fb341650d73e808088bb2800312de4 |
| Total download size | 610.66 MB (urchade/gliner_small-v2.1: gliner_config.json + pytorch_model.bin + README.md + .gitattributes = 477 B + 610,652,234 B + 4,756 B + 1,519 B = 610,658,986 B) |

## Fetch the model

Warm both the GLiNER model and its base encoder at pinned revisions:

```sh
hf download urchade/gliner_small-v2.1 --revision 4e091416cf7c3481db542c2a3d26156916f3a47f
hf download microsoft/deberta-v3-small --revision a36c739020e01763fe789b4b85e2df55d6180012
```

Expected output:
```
Warning: You are sending unauthenticated requests to the HF Hub. Please set a HF_TOKEN to enable higher rate limits and faster downloads.
path=/Users/iansmith/.cache/huggingface/hub/models--urchade--gliner_small-v2.1/snapshots/4e091416cf7c3481db542c2a3d26156916f3a47f
path=/Users/iansmith/.cache/huggingface/hub/models--microsoft--deberta-v3-small/snapshots/a36c739020e01763fe789b4b85e2df55d6180012
```

## Verify the model

Recompute the sha256 of the downloaded pytorch_model.bin:

```sh
shasum -a 256 ~/.cache/huggingface/hub/models--urchade--gliner_small-v2.1/snapshots/4e091416cf7c3481db542c2a3d26156916f3a47f/pytorch_model.bin
# 1d4e83e4e4ae4ae0a4fbc81a32ee6de480fb341650d73e808088bb2800312de4
```

## Model weights

The model weights (pytorch_model.bin, 610.7 MB) are **not committed** to this repository. GitHub enforces a 100 MB hard limit per file. Download and cache locally as shown above; the sidecar at AATK-53 will load from the HF cache at runtime.
