package telephony

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// pendingSettleTimeout bounds Replay's wait for outstanding STT round trips
// (see pendingSettle's comment in Replay). Generous relative to a real
// whisper sidecar's transcription latency (seconds, not this), so it never
// fires on a genuine in-flight request -- only on the rare dropped-dispatch
// case, which has no other completion signal to wait on.
const pendingSettleTimeout = 30 * time.Second

// replayTimerBoundMS overrides all three of production's wall-clock caller
// timers (MaxSilenceMS, MaxUtteranceMS, MaxTurnMS -- state.go) for the
// duration of a Replay call -- see the WithMax*MS options in Replay's
// defaultOpts for why a replay needs much longer bounds than a live call
// does.
const replayTimerBoundMS = 10 * 60 * 1000

// ReplayResult is one utterance's FullPass transcript from a replay run
// (ticket SOP-153 Observable behavior #1: "emits the ordered FullPass
// transcripts as JSON" -- one per utterance, the same granularity `build`
// structures its cumulative-prefix rows over, not one per fused turn).
type ReplayResult struct {
	Text string `json:"text"`
}

// replayFrameBytes is the chunk size Replay slices audioStream into before
// sending each chunk to the session's data-plane input. Session.handleDataFrame
// places no framing requirement on its input (it appends whatever byte slice
// it's given to the turn buffer and forwards it toward the VAD windower
// unchanged), so this only needs to be *a* reasonable chunk size -- chosen to
// match defaultVADConfig().WindowSize (256), the same convention this
// package's own tests use (session_test.go's windowFrame helper).
const replayFrameBytes = 256

// replaySilenceByte is the μ-law byte value used for the synthetic trailing
// padding Replay appends after audioStream reaches EOF (see feedReplaySilencePadding).
// Matches the silence byte session_test.go's windowFrame(0x80) helper uses
// throughout this package's own test suite.
const replaySilenceByte = 0x80

// silencePaddingMargin adds a few extra windows beyond the exact threshold
// crossing so rounding in windowsToCross can never leave the VAD one window
// short of declaring end-of-utterance/turn-end. It also does a second,
// load-bearing job: see the compile-time assertion below replayLookahead's
// declaration.
const silencePaddingMargin = 4

// utteranceRecorder is an unbuffered STTOutput that also records each
// result's Text, in dispatch order, as Replay's own output. This -- not
// TurnSink.OnTurnComplete -- is Replay's source of truth for "the ordered
// FullPass transcripts": OnTurnComplete delivers turn-*fused* text
// (StopwordPolicy swallows the "done" utterance entirely, and multiple
// utterances joined by silence-turn-end collapse into one fused string),
// a different, coarser granularity than the per-utterance FullPass
// transcripts Observable behavior #1 and `build`'s cumulative-prefix rows
// are both defined over.
//
// Send being unbuffered means it blocks until Session's run loop actually
// takes the value off the channel; since that loop dispatches a received
// STTResult synchronously before its next select iteration, Send returning
// guarantees Session has already processed it. wg.Done() fires only after
// texts is appended (not merely after the channel handoff), so a
// pending.Wait() in Replay can never observe fewer results than were
// actually recorded -- the real completion signal that replaces the fixed
// sleep this type exists to fix (SOP-153 finding #4).
type utteranceRecorder struct {
	ch chan STTResult
	wg *sync.WaitGroup

	mu    sync.Mutex
	texts []string
}

func newUtteranceRecorder(wg *sync.WaitGroup) *utteranceRecorder {
	return &utteranceRecorder{ch: make(chan STTResult), wg: wg}
}

func (u *utteranceRecorder) Channel() <-chan STTResult { return u.ch }

func (u *utteranceRecorder) Send(ctx context.Context, v STTResult) error {
	select {
	case u.ch <- v:
		u.mu.Lock()
		u.texts = append(u.texts, v.Text)
		u.mu.Unlock()
		u.wg.Done()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (u *utteranceRecorder) Recv(ctx context.Context) (STTResult, error) {
	select {
	case v := <-u.ch:
		return v, nil
	case <-ctx.Done():
		var zero STTResult
		return zero, ctx.Err()
	}
}

func (u *utteranceRecorder) results() []ReplayResult {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]ReplayResult, len(u.texts))
	for i, text := range u.texts {
		out[i] = ReplayResult{Text: text}
	}
	return out
}

// replayLookahead bounds how many frames feedReplayAudio/feedReplaySilencePadding
// may have sent that the VAD hasn't yet fully settled (see vadProgressGate).
// It matches forwardCh's depth (session.go: ComputeDepth(DataPlaneBufferMS,
// MuLawFrameMS) == 4) -- handleDataFrame's send into forwardCh is the one
// non-blocking, drop-on-full point in this pipeline (forwardToVAD's own
// relay into the VAD service, and the VAD service's internal consumption,
// are both blocking sends). Keeping the producer within this many frames of
// confirmed settlement means forwardCh can never see more in flight than it
// was sized for, so it never has a reason to drop.
const replayLookahead = 4

// This assertion is the other reason silencePaddingMargin must never shrink
// below replayLookahead: Replay's WaitGroup ordering proof (pending.Add()
// for the final utterance's end-of-utterance happens-before pending.Wait())
// relies on settlement being strictly FIFO and on feedReplaySilencePadding
// sending at least replayLookahead trailing-silence windows past the
// threshold crossing before Replay ever calls pending.Wait() -- window
// W+silencePaddingMargin cannot be sent until window W has settled (the
// gate enforces exactly that), so if the margin were ever smaller than the
// lookahead, the padding loop could finish -- and Replay proceed to
// pending.Wait() -- before the final EOU's settlement, and therefore before
// its pending.Add(), has actually happened. That would silently
// reintroduce the exact race this ticket's fixed-sleep removal exists to
// eliminate, with no test likely to catch it except a rare flake. A
// negative result here fails the build (assigning a negative constant to
// uint), not the test suite.
const _ = uint(silencePaddingMargin - replayLookahead)

// vadProgressGate is real backpressure between feedReplayAudio's sends and
// the VAD's actual per-window settlement, replacing a guessed pacing
// interval. It starts pre-loaded with replayLookahead tokens; acquire
// blocks until a token is available (i.e. until some earlier window has
// fully settled -- see WithVADEventObserver in session.go), and release
// returns one. This bounds how far ahead of genuine consumption the
// producer can ever get, independent of how fast or slow the VAD pipeline
// happens to run (real Silero under -race is measurably, variably slower
// than a fake test detector, which a fixed-duration pace guess would have
// to either overshoot for or under-provision against; a token acquired from
// real progress needs neither).
type vadProgressGate struct {
	tokens chan struct{}
}

func newVADProgressGate(n int) *vadProgressGate {
	g := &vadProgressGate{tokens: make(chan struct{}, n)}
	for i := 0; i < n; i++ {
		g.tokens <- struct{}{}
	}
	return g
}

func (g *vadProgressGate) acquire(ctx context.Context) error {
	select {
	case <-g.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *vadProgressGate) release() {
	select {
	case g.tokens <- struct{}{}:
	default: // already full -- more releases than acquires can't happen, but never block on it
	}
}

// Replay drives audioStream (typically a captured .ulaw's bytes) through a
// production Session -- the same VAD+STT path a live call runs, not a copy
// -- and returns the ordered FullPass turn transcripts.
//
// sttClient is the same STTClient production wires (internal/telephony/stt.go);
// callers pass a real one (NewSTTClient(sttBaseURL)) to drive a genuine
// whisper sidecar, or one pointed at an httptest server for deterministic
// tests -- either way it is the identical STTClient.Transcribe code path,
// never a stand-in.
//
// opts overrides Session construction after Replay's own defaults (turn
// sink, data/control/STT wiring, StopwordPolicy) are applied, so a caller
// can inject e.g. WithVADConfig to change --end-silence-ms, or WithVADFactory
// in tests that don't need the real Silero model.
//
// Replay is deterministic: for fixed audioStream bytes, a fixed sttClient
// response set, and fixed opts, two calls produce byte-identical results,
// every run -- no wall-clock sleep gates any part of this function.
func Replay(ctx context.Context, callSID string, audioStream io.Reader, sttClient *STTClient, opts ...SessionOption) ([]ReplayResult, error) {
	dataIn := NewBufferedChan[[]byte](16)
	controlIn := NewBufferedChan[ControlEvent](1)

	// pending tracks the whole VADEndOfUtterance-to-STTResult round trip as
	// ONE unit, end to end, in place of a guessed sleep (SOP-153 finding
	// #4): eventObserver adds one the instant an event is
	// guaranteed-delivered onto the session's VAD output channel (run()'s
	// select is now guaranteed to see it) -- whether or not it will
	// actually result in an STT dispatch (state.go's
	// handleAwaitingFullResultVADEvent calls onUtteranceEnd, dropping the
	// dispatch, when a second utterance's end arrives while the first
	// pass is still in flight; that add is never matched by a done and is
	// covered by pendingSettle's bounded backstop below, not treated as
	// the common case). utteranceRecorder.Send does the matching done,
	// once the STTResult has actually reached run() (see its own comment).
	// gate.release() is called from the same callback as eventObserver's
	// add, in the same program order, so a fully-drained gate always
	// implies every add that will ever happen for the windows fed so far
	// has already happened.
	sttReqCh := NewBufferedChan[STTRequest](16)
	var pending sync.WaitGroup
	recorder := newUtteranceRecorder(&pending)

	svc := NewSTTService(sttClient, sttReqCh, recorder)
	sttCtx, sttCancel := context.WithCancel(ctx)
	defer sttCancel()
	go StartSTTService(sttCtx, svc)

	gate := newVADProgressGate(replayLookahead)
	eventObserver := func(ev VADEvent, emitted bool) {
		if emitted && ev.Kind == VADEndOfUtterance {
			pending.Add(1)
		}
		gate.release()
	}

	defaultOpts := []SessionOption{
		WithTwilioDataInput(dataIn),
		WithTwilioControlInput(controlIn),
		WithSTTInput(sttReqCh),
		WithSTTOutput(recorder),
		WithTurnEndPolicy(StopwordPolicy{}),
		WithVADEventObserver(eventObserver),
		// Replay's own feeding pace (paceReplayWindow, real-time per window)
		// is about reproducing a live call's inter-utterance timing, not
		// about how long *feeding a whole recording* takes in wall-clock
		// terms -- which, under load (e.g. -race's instrumentation overhead
		// slowing real Silero inference measurably), can exceed any of
		// production's three caller-facing timers (idle, per-utterance,
		// per-turn) despite the audio itself containing nothing but what
		// was actually captured. All three are wall-clock timers wired
		// against real time regardless of WithClock (none of them read
		// through the session's clock seam), so the only way to keep a
		// slow-running replay from tripping one mid-feed is to raise their
		// bounds for the duration of this call.
		WithMaxSilenceMS(replayTimerBoundMS),
		WithMaxUtteranceMS(replayTimerBoundMS),
		WithMaxTurnMS(replayTimerBoundMS),
	}
	session := NewSession(ctx, callSID, append(defaultOpts, opts...)...)

	if err := session.Start(); err != nil {
		return nil, fmt.Errorf("replay: start session: %w", err)
	}
	defer session.Close()

	if err := feedReplayAudio(ctx, dataIn, audioStream, gate); err != nil {
		return nil, fmt.Errorf("replay: feed audio: %w", err)
	}
	if err := feedReplaySilencePadding(ctx, dataIn, session.vadCfg, gate); err != nil {
		return nil, fmt.Errorf("replay: feed padding: %w", err)
	}

	// pendingSettle bounds the one case pending's own accounting can never
	// resolve by itself: an eventObserver add for a VADEndOfUtterance that
	// state.go's handleAwaitingFullResultVADEvent drops without ever
	// dispatching (see pending's comment above), which has no done to
	// balance it. That's a genuine, if rare, real-call scenario (a second
	// utterance closing before the first pass's STT round trip returns) --
	// not a fixed guess at STT latency, which every dispatch that DOES
	// happen still waits out via pending.Wait() below with no timeout.
	// This bound only ever matters for the drop; it never fires early on
	// a real completion, because a real completion always signals through
	// pending.Wait() first.
	settled := make(chan struct{})
	go func() {
		pending.Wait()
		close(settled)
	}()
	select {
	case <-settled:
	case <-time.After(pendingSettleTimeout):
	case <-ctx.Done():
	}

	// Mirrors the real call-end signal (Twilio's "stop" control message,
	// handleControlEvent in state.go) so Session tears down through the
	// same path a live call's hangup does, rather than Close() alone
	// cancelling the context out from under a session that never saw its
	// call end cleanly.
	if err := controlIn.Send(ctx, ControlEvent{Kind: controlKindStop, CallSID: callSID}); err != nil {
		return nil, fmt.Errorf("replay: send call-end: %w", err)
	}

	return recorder.results(), nil
}

// feedReplayAudio slices audioStream into replayFrameBytes-sized chunks and
// sends each into dataIn in order, EOF-terminated, acquiring one gate token
// before every send. Bounded by the gate (see vadProgressGate) rather than a
// guessed pacing interval: forwardCh (session.go) -- the handoff from
// Session's data-plane input to the VAD goroutine -- is a small, fixed-depth
// buffer with a non-blocking, drop-on-full send, and blasting an entire
// recording's frames in with no throttling races it, silently dropping
// windows and corrupting the VAD's view of the audio (directly threatening
// Observable behavior #1's determinism and the fidelity gate).
func feedReplayAudio(ctx context.Context, dataIn TwilioDataPlaneInput, audioStream io.Reader, gate *vadProgressGate) error {
	buf := make([]byte, replayFrameBytes)
	for {
		n, err := audioStream.Read(buf)
		if n > 0 {
			if gateErr := gate.acquire(ctx); gateErr != nil {
				return gateErr
			}
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if sendErr := dataIn.Send(ctx, chunk); sendErr != nil {
				return sendErr
			}
			if paceErr := paceReplayWindow(ctx); paceErr != nil {
				return paceErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// replayWindowMS is the wall-clock duration one replayFrameBytes chunk
// represents at the production sample rate (256 samples @ 8kHz = 32ms) --
// the same cadence Twilio actually delivers frames at, and what
// paceReplayWindow throttles feedReplayAudio/feedReplaySilencePadding to.
//
// This is a second, independent reason to throttle sends, distinct from
// vadProgressGate's buffer-safety one: without it, a caller with two
// utterances separated by a pause well under whisper's real ~1s-per-pass
// STT latency (stt.go's defaultSTTTimeout doc) can have its second
// utterance's VADEndOfUtterance reach run() while the first utterance's
// STT round trip is still in flight -- state.go's
// handleAwaitingFullResultVADEvent drops the second dispatch entirely in
// that case, same as it would on a live call receiving audio faster than
// real time. Pacing at the real per-window duration reproduces the same
// real-time cushion between utterances a live call actually has (Twilio
// only ever delivers audio this fast), which is what keeps that drop path
// as rare in replay as it is in production instead of common.
const replayWindowMS = replayFrameBytes * 1000 / SampleRateHz

func paceReplayWindow(ctx context.Context) error {
	t := time.NewTimer(replayWindowMS * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// feedReplaySilencePadding appends enough silent frames, at cfg's actual
// (already-defaulted) EndSilenceMS, to force the VAD to close out any
// utterance still open when audioStream reached EOF -- so replay's final
// utterance dispatches its FullPass request the same way a live call's
// would, instead of being silently dropped for lack of trailing silence in
// the captured recording. Gated the same way feedReplayAudio is, for the
// same buffer-safety reason.
func feedReplaySilencePadding(ctx context.Context, dataIn TwilioDataPlaneInput, cfg vadConfig, gate *vadProgressGate) error {
	cfg = cfg.withDefaults()
	windows := windowsToCross(cfg.EndSilenceMS, cfg.windowMS()) + silencePaddingMargin
	frame := make([]byte, replayFrameBytes)
	for i := range frame {
		frame[i] = replaySilenceByte
	}
	for i := 0; i < windows; i++ {
		if err := gate.acquire(ctx); err != nil {
			return err
		}
		if err := dataIn.Send(ctx, frame); err != nil {
			return err
		}
		if err := paceReplayWindow(ctx); err != nil {
			return err
		}
	}
	return nil
}

// RecordingLabel is the vocabulary a captured recording is tagged with at
// capture time (AATOOLKIT_TAP_LABEL, twilio/tap.go's sidecar "label" field),
// read back here to derive each recording's dataset rows structurally
// instead of by hand.
type RecordingLabel string

const (
	LabelCompleteTurn RecordingLabel = "complete-turn"
	LabelTruncated    RecordingLabel = "truncated"
	LabelGreeting     RecordingLabel = "greeting"
	LabelHowAreYou    RecordingLabel = "how-are-you"
	LabelDoneEnding   RecordingLabel = "done-ending"
)

// RowLabel is a dataset row's structurally-derived completeness label.
type RowLabel string

const (
	RowIncomplete RowLabel = "incomplete"
	RowComplete   RowLabel = "complete"
)

// HarnessVersion tags every row this build of the harness produces (DoD:
// "rows carry provenance: recording id, VAD config, harness version").
const HarnessVersion = "SOP-153-v1"

// DatasetRow is one cumulative-prefix row of a replayed recording's FullPass
// transcripts, labelled structurally rather than by hand.
type DatasetRow struct {
	RecordingID    string   `json:"recording_id"`
	Utterances     []string `json:"utterances"`
	Label          RowLabel `json:"label"`
	EndSilenceMS   int      `json:"end_silence_ms"`
	HarnessVersion string   `json:"harness_version"`
}

// RowsFromUtterances builds one cumulative-prefix DatasetRow per utterance in
// utterances (u1, u1+u2, ... u1..un), each labelled structurally from
// recLabel: every prefix short of the full recording is `incomplete` by
// construction -- the caller demonstrably kept talking past it, which is not
// a judgment call. The final, full-recording prefix is `complete`, unless
// recLabel is `truncated`, in which case the recording never reached a
// confirmed end and every row -- including the terminal one -- is
// `incomplete` (ticket SOP-153 "Why": labels are structural, not
// hand-written; re-running at a different --end-silence-ms produces a
// different, equally correct set because the utterances it structures over
// change with the VAD config).
func RowsFromUtterances(recordingID string, recLabel RecordingLabel, utterances []string, endSilenceMS int) []DatasetRow {
	rows := make([]DatasetRow, 0, len(utterances))
	for i := range utterances {
		label := RowIncomplete
		isTerminal := i == len(utterances)-1
		if isTerminal && recLabel != LabelTruncated {
			label = RowComplete
		}
		rows = append(rows, DatasetRow{
			RecordingID:    recordingID,
			Utterances:     append([]string(nil), utterances[:i+1]...),
			Label:          label,
			EndSilenceMS:   endSilenceMS,
			HarnessVersion: HarnessVersion,
		})
	}
	return rows
}

// VerifierOutcome is one of the four outcomes score reports for a row, in
// place of raw accuracy (ticket SOP-153 Observable behavior #5: accuracy
// averages over an asymmetry where one error truncates a caller and the
// other costs a re-ask).
type VerifierOutcome string

const (
	OutcomeProceed         VerifierOutcome = "proceed"
	OutcomeSpuriousRepair  VerifierOutcome = "spurious_repair"
	OutcomeRepairFires     VerifierOutcome = "repair_fires"
	OutcomePartialAccepted VerifierOutcome = "partial_accepted"
)

// Verifier runs promptFile's prompt over one dataset row and reports which
// of the four outcomes it produced. cmd/probeset's real score wiring
// supplies one backed by an actual model call; tests supply a fixed fake, so
// Score's own aggregation logic is exercised independent of any live prompt
// call.
type Verifier func(ctx context.Context, promptFile string, row DatasetRow) (VerifierOutcome, error)

// ScoreReport is score's output: which prompt ran, at which VAD config, over
// how many rows, and how many rows landed in each of the four outcomes.
// Deliberately carries no accuracy field (DoD: "score reports the four
// outcomes, not accuracy").
type ScoreReport struct {
	PromptFile   string                  `json:"prompt_file"`
	EndSilenceMS int                     `json:"end_silence_ms"`
	RowCount     int                     `json:"row_count"`
	Outcomes     map[VerifierOutcome]int `json:"outcomes"`
}

// Score runs verify over every row and tallies the four named outcomes.
func Score(ctx context.Context, promptFile string, rows []DatasetRow, endSilenceMS int, verify Verifier) (ScoreReport, error) {
	report := ScoreReport{
		PromptFile:   promptFile,
		EndSilenceMS: endSilenceMS,
		RowCount:     len(rows),
		Outcomes: map[VerifierOutcome]int{
			OutcomeProceed:         0,
			OutcomeSpuriousRepair:  0,
			OutcomeRepairFires:     0,
			OutcomePartialAccepted: 0,
		},
	}
	for _, row := range rows {
		outcome, err := verify(ctx, promptFile, row)
		if err != nil {
			return report, fmt.Errorf("score: verify row %s: %w", row.RecordingID, err)
		}
		report.Outcomes[outcome]++
	}
	return report, nil
}
