// Package assets embeds vendored μ-law audio clips, so a caller needs no
// runtime file read to load them.
package assets

import _ "embed"

// FarewellULaw is the call-termination farewell clip, μ-law
// encoded at 8 kHz (1 byte/sample, so len/8000 == seconds). It says
// "Call me anytime... Bye!" in the voice-out TTS's F5 voice.
//
// voice-out is a supertonic TTS sidecar, the kind of /v1/tts endpoint the
// driver package's Speak synthesizes through, so an agent whose replies are
// voiced in voice-out's F5 voice ends the call in the voice it spoke in.
//
// A consumer can play it at the end of a realtime call by passing it to
// twilio.WithFarewellAudio. PROVENANCE.md records how to regenerate it.
// Nothing depends on its length: the wait for the carrier's mark echo after
// the clip is derived from the audio queued ahead of the mark, so a longer or
// shorter clip retimes itself.
//
//go:embed farewell.ulaw
var FarewellULaw []byte

// AudioForcedStopULaw is the forced-stop clip, μ-law encoded at
// 8 kHz (1 byte/sample). The engine has no playback path for it: the call
// pipeline that played it when a caller overran a single-utterance cap was
// removed (AATK-164).
//
//go:embed audio-forced-stop.ulaw
var AudioForcedStopULaw []byte

// LLMThinkingULaw is a loopable "thinking" bed, μ-law at 8 kHz. It was
// first embedded with no playback path, for a future LLM-composed reply
// to loop while it waits for the real response.
//
//go:embed llm-thinking.ulaw
var LLMThinkingULaw []byte
