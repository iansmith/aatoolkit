// Package assets embeds vendored binary model assets so the telephony
// package never needs a runtime file read to load them.
package assets

import _ "embed"

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
// embedded it with no playback path yet, for a future LLM-composed reply
// (SOP-115/G,H) to loop while it waits for the real response.
//
//go:embed llm-thinking.ulaw
var LLMThinkingULaw []byte
