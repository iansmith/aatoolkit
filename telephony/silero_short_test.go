package telephony

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSilero_DetectsShortUtterance feeds a short (~1.5 s) clear utterance —
// how_are_you.ulaw, "How are you doing today?" — through the real detector and
// windower and asserts the model's speech probability crosses SpeechThresh at
// least once, so the VAD would emit a speech onset.
//
// AATK-8: Silero VAD at 8 kHz requires a 64-sample context (the tail of the
// previous chunk) prepended to each 256-sample window. Without it the model runs
// cold every frame and its probability only ramps up over ~2 s of sustained
// speech — so on a ~1.5 s utterance it never passes ~0.28, the onset never fires,
// and the utterance is invisible to the pipeline (probeset yields no rows for the
// greeting/how-are-you takes). With the context it fires at the first loud frame.
func TestSilero_DetectsShortUtterance(t *testing.T) {
	ulaw, err := os.ReadFile(filepath.Join("testdata", "how_are_you.ulaw"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	det, err := NewSileroDetector()
	if err != nil {
		t.Fatalf("NewSileroDetector: %v", err)
	}
	cfg := defaultVADConfig()
	w := newWindower(cfg.WindowSize)

	const chunkSize = 160 // Twilio 20 ms mu-law chunk @ 8 kHz — matches runRealPipeline
	var maxProb float32
	for off := 0; off < len(ulaw); off += chunkSize {
		end := off + chunkSize
		if end > len(ulaw) {
			end = len(ulaw)
		}
		for _, win := range w.push(decodeMuLaw(ulaw[off:end])) {
			prob, err := det.Detect(win)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if prob > maxProb {
				maxProb = prob
			}
		}
	}

	if maxProb < cfg.SpeechThresh {
		t.Errorf("max speech prob %.3f < SpeechThresh %.3f — short utterance not detected; "+
			"the Silero 64-sample context window is missing (AATK-8)", maxProb, cfg.SpeechThresh)
	}
}
