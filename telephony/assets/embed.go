// Package assets embeds vendored binary model assets so the telephony
// package never needs a runtime file read to load them.
package assets

import _ "embed"

// SileroVADONNX is the vendored Silero VAD v6.2.1 ONNX model (see
// PROVENANCE.md). It is byte-identical to
// third_party/gonnx/sample_models/onnx_models/silero_vad.onnx —
// TestModelDriftGuard in internal/telephony/silero_test.go enforces that.
//
//go:embed silero_vad.onnx
var SileroVADONNX []byte

// FarewellULaw is the call-termination farewell clip (SOP-125), μ-law
// encoded at 8 kHz (1 byte/sample, so len/8000 == seconds). It says
// "Call me anytime... Bye!" in the voice-out TTS's F5 voice.
//
// voice-out is the engine that will speak the engine's replies once SOP-115/G,H
// builds that path, so sourcing the farewell from it means she will end a
// call in the voice she spoke it in. Today it is the only thing she says:
// handleSTTResult logs a transcript and returns, and nothing in this package
// talks to voice-out at all.
//
// Regenerate with scripts/make_farewell.sh (see PROVENANCE.md). Nothing
// depends on its length: MarkEchoTimeout derives the post-playout wait from
// len(FarewellULaw), so a longer or shorter clip retimes itself.
//
//go:embed farewell.ulaw
var FarewellULaw []byte

// AudioForcedStopULaw is the forced-stop clip (SOP-156), μ-law encoded at
// 8 kHz (1 byte/sample). It plays when a single utterance exceeds
// MaxUtteranceMS and the caller is cut off, then the call terminates through
// the same mark-echo flow as the farewell. Like FarewellULaw, nothing depends
// on its length: MarkEchoTimeout derives the post-playout wait from it.
//
//go:embed audio-forced-stop.ulaw
var AudioForcedStopULaw []byte

// LLMThinkingULaw is a loopable "thinking" bed, μ-law at 8 kHz. SOP-156
// embedded it with no playback path yet; SOP-157 plays it on the outbound
// data plane during the sim-turn collection stand-in (Session.sendBed),
// looping to fill the configured duration ahead of the real LLM-composed
// reply (SOP-115/G,H), which will eventually replace this fixed clip.
//
//go:embed llm-thinking.ulaw
var LLMThinkingULaw []byte
