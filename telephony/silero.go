package telephony

// Silero VAD v6.2.1 wire constants, shared by the HTTP detector
// (silero_http.go) and the VAD windowing in vad.go. Inference itself runs
// out-of-process in the VAD sidecar (scripts/vad_server.py) reached over HTTP;
// the engine holds no in-process ONNX runtime (SOP-147 retired the in-process
// ONNX detector — see silero_http.go).

// sileroWindowSize is the Silero VAD model's per-call chunk: 256 float32 PCM
// samples (32ms @ 8kHz).
const sileroWindowSize = 256

// sileroContextSize is the number of samples of the PREVIOUS window Silero VAD
// carries as context, prepended to each new chunk (AATK-8). At 8kHz the model's
// actual input is sileroContextSize + sileroWindowSize = 64 + 256 = 320 samples;
// feeding a bare 256 (no context) runs the model cold each frame and misses short
// utterances — see snakers4/silero-vad and testdata/how_are_you.ulaw.
const sileroContextSize = 64

// sileroStateShape is the model's recurrent state tensor shape: 2 LSTM
// layers, batch size 1, hidden size 128.
var sileroStateShape = []int{2, 1, 128}

// sileroStateElems is the flattened length of the recurrent state tensor — one
// definition shared with the HTTP detector's wire encoding (silero_http.go), so
// the two can't drift.
func sileroStateElems() int {
	return sileroStateShape[0] * sileroStateShape[1] * sileroStateShape[2]
}
