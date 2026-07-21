package telephony

import "testing"

// AATK-3: the default end-of-utterance silence is 900ms, raised from 700ms. At 700 the
// VAD cut an utterance boundary mid-clause and Whisper then hallucinated an ending on the
// truncated fragment (dataset recording D5, "the plumber still hasn't called back" →
// "has to do it"); at 900 the sentence stays whole. This pins the tuned value so an
// accidental revert to 700 is caught. The companion behavioral check is the shifted
// end-of-utterance window (237→244) in TestSileroE2ETimelineGolden.
func TestDefaultVADConfig_EndSilenceMS(t *testing.T) {
	if got := DefaultVADConfig().EndSilenceMS; got != 900 {
		t.Errorf("DefaultVADConfig().EndSilenceMS = %d, want 900", got)
	}
}
