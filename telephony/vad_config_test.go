package telephony

import "testing"

// AATK-9: the default end-of-utterance silence is 700ms, reverted from 900ms. AATK-3 had
// raised it to 900 to stop a Whisper phrase-break hallucination (dataset D5, "the plumber
// still hasn't called back" → "has to do it"), but that hallucination was the AATK-8
// cold-start VAD bug (Silero fed a bare 256-sample window with no 64-sample context), not a
// 700 effect. With AATK-8 fixed, a 700/900/1050 re-sweep over the fix-clean set is
// content-identical, so 700 is restored (SOP-154's value, lower turn-taking latency). This
// pins the tuned value so an accidental change is caught. The companion behavioral check is
// TestSileroE2ETimelineGolden, which asserts the pipeline's end-of-utterance window against
// the committed golden.
//
// 700 is an irreducible *decision* value — the tuned choice itself, not a quantity derived
// from other constants — so it is legitimately a literal here (cf. window counts, which
// derive from EndSilenceMS via EndSilenceWindows()).
func TestDefaultVADConfig_EndSilenceMS(t *testing.T) {
	if got := DefaultVADConfig().EndSilenceMS; got != 700 {
		t.Errorf("DefaultVADConfig().EndSilenceMS = %d, want 700", got)
	}
}
