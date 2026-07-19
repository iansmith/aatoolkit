// Package aatoolkit is an open, reusable toolkit for building conversational AI
// assistants: telephony (Twilio media streams), speech-to-text and voice-activity
// detection, LLM transport, a dynamic-Go policy loader for hot-reloadable behavior,
// and a fact-database toolkit for extracting and storing knowledge in a graph.
//
// It provides mechanism, not a finished assistant. The particular behavior, identity,
// and policy of any given assistant live in a separate, private repository that
// depends on this module — never the reverse.
//
// Extraction of the reusable packages from their original monorepo is in progress;
// see design/ for the architecture and the split plan.
package aatoolkit
