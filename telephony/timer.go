package telephony

import (
	"context"
	"time"
)

// TimerCompletion represents the result of a completed timer.
type TimerCompletion struct {
	Name     string
	TimerID  int
	Duration time.Duration
}

// TimerFacility manages multiple named timers with generation counters.
// It is NOT thread-safe for direct manipulation, but uses channels for
// safe coordination between goroutines.
type TimerFacility struct {
	ctx          context.Context
	completions  chan TimerCompletion
	commands     chan timerCommand
	nextTimerID  int
	activeTimers map[string]int // name -> current generation counter
	cancelFuncs  map[string]context.CancelFunc

	// after is how a timer waits out its duration. It is a field, not a
	// direct call to time.After, so a caller can supply the passage of time
	// instead of the facility reading the wall clock itself.
	//
	// This is what makes timer-driven behavior testable precisely: without it
	// the only way to observe a timer firing is to sleep past its deadline,
	// and a test that sleeps is asserting on the scheduler. Worse, such a
	// test still passes when the deadline it was meant to outlast grows
	// beyond the sleep -- it just stops proving anything.
	//
	// nil means time.After (see NewTimerFacility).
	after func(time.Duration) <-chan time.Time

	// settledTimers is a FIFO of (name, generation) pairs that have already
	// fired. IsCurrent must keep returning true for a generation immediately
	// after its completion is delivered (TestTimerFacilityRearm depends on
	// this), so a fired entry cannot be deleted from activeTimers right away.
	// Instead it queues here and is evicted once the queue grows past
	// maxSettledTimers, bounding growth for callers that arm unique
	// never-repeated names.
	settledTimers []settledTimer
}

type settledTimer struct {
	name string
	gen  int
}

const maxSettledTimers = 1024

// timerCommand represents a command to the facility's run loop.
type timerCommand struct {
	cmd interface{}
}

type armCmd struct {
	name     string
	duration time.Duration
	resultCh chan int
}

type cancelCmd struct {
	name string
}

type isCurrentCmd struct {
	name     string
	timerID  int
	resultCh chan bool
}

type fireTimerIfCurrentCmd struct {
	name     string
	timerID  int
	duration time.Duration
}

// NewTimerFacility creates a new timer facility with the given context.
// When the context is cancelled, all armed timers' goroutines will exit.
func NewTimerFacility(ctx context.Context) *TimerFacility {
	return NewTimerFacilityWithClock(ctx, time.After)
}

// NewTimerFacilityWithClock builds a facility whose timers wait via after
// instead of the wall clock. Production passes time.After (NewTimerFacility);
// tests pass a fake so a timer fires exactly when the test says it does,
// rather than when the scheduler gets round to it.
func NewTimerFacilityWithClock(ctx context.Context, after func(time.Duration) <-chan time.Time) *TimerFacility {
	if after == nil {
		after = time.After
	}
	tf := &TimerFacility{
		after:       after,
		ctx:         ctx,
		completions: make(chan TimerCompletion, 1), // buffered to avoid blocking sends
		// commands is unbuffered: a send only completes in lockstep with run()
		// actually receiving it in the same select, so a command can never be
		// stranded in a buffer if run() exits via ctx.Done() at the same instant.
		commands:     make(chan timerCommand),
		nextTimerID:  1,
		activeTimers: make(map[string]int),
		cancelFuncs:  make(map[string]context.CancelFunc),
	}

	// Start the facility's run loop in a goroutine
	go tf.run()

	return tf
}

// Arm starts a timer with the given name and duration. It returns a generation
// counter (timerId). If a timer with this name is already armed, it cancels
// the previous one first. Returns -1 if the facility's context has been cancelled.
func (tf *TimerFacility) Arm(ctx context.Context, name string, duration time.Duration) int {
	resultCh := make(chan int)
	select {
	case tf.commands <- timerCommand{&armCmd{
		name:     name,
		duration: duration,
		resultCh: resultCh,
	}}:
		return <-resultCh
	case <-tf.ctx.Done():
		return -1
	case <-ctx.Done():
		return -1
	}
}

// Cancel cancels the timer with the given name. If the timer's completion
// is already pending, this prevents it from being sent. Cancelling a
// non-existent timer is a no-op. Post-teardown calls are silently ignored.
func (tf *TimerFacility) Cancel(name string) {
	select {
	case tf.commands <- timerCommand{&cancelCmd{name: name}}:
	case <-tf.ctx.Done():
		// Facility's context is cancelled — no-op
	}
}

// Completions returns a read-only channel that receives TimerCompletion
// events when armed timers fire.
func (tf *TimerFacility) Completions() <-chan TimerCompletion {
	return tf.completions
}

// IsCurrent checks whether the given completion's TimerID is the current
// generation for its name. This allows consumers to distinguish stale
// fires from current ones. Returns false if the facility's context is cancelled.
func (tf *TimerFacility) IsCurrent(completion TimerCompletion) bool {
	resultCh := make(chan bool)
	select {
	case tf.commands <- timerCommand{&isCurrentCmd{
		name:     completion.Name,
		timerID:  completion.TimerID,
		resultCh: resultCh,
	}}:
		return <-resultCh
	case <-tf.ctx.Done():
		return false
	}
}

// run is the facility's main loop. It handles commands (Arm, Cancel, IsCurrent) and
// coordinates timer goroutines via cancellation contexts.
func (tf *TimerFacility) run() {
	for {
		select {
		case <-tf.ctx.Done():
			// Facility's context is cancelled — cancel all active timers
			for _, cancel := range tf.cancelFuncs {
				cancel()
			}
			tf.cancelFuncs = make(map[string]context.CancelFunc)
			return

		case cmd := <-tf.commands:
			switch c := cmd.cmd.(type) {
			case *armCmd:
				tf.handleArm(c)
			case *cancelCmd:
				tf.handleCancel(c.name)
			case *isCurrentCmd:
				tf.handleIsCurrent(c)
			case *fireTimerIfCurrentCmd:
				tf.handleFireTimerIfCurrent(c)
			}
		}
	}
}

func (tf *TimerFacility) handleArm(cmd *armCmd) {
	// Cancel any existing timer for this name
	if cancelFunc, exists := tf.cancelFuncs[cmd.name]; exists {
		cancelFunc()
		delete(tf.cancelFuncs, cmd.name)
	}

	// Increment the generation counter
	tf.nextTimerID++
	timerId := tf.nextTimerID
	tf.activeTimers[cmd.name] = timerId

	// Create a cancellable context for this timer
	timerCtx, cancel := context.WithCancel(tf.ctx)
	tf.cancelFuncs[cmd.name] = cancel

	// Return the generation counter to the caller
	cmd.resultCh <- timerId

	// Start a goroutine for this timer
	go tf.timerGoroutine(cmd.name, timerId, cmd.duration, timerCtx, cancel)
}

func (tf *TimerFacility) handleCancel(name string) {
	if cancelFunc, exists := tf.cancelFuncs[name]; exists {
		cancelFunc()
		delete(tf.cancelFuncs, name)
	}
	// No timer is active for this name anymore, so it no longer needs an
	// entry: unlike re-arm (which overwrites), a cancelled-and-never-rearmed
	// name would otherwise sit in the map forever.
	delete(tf.activeTimers, name)
}

// evictSettledTimers bounds activeTimers' growth by dropping the oldest
// fired-and-settled generations once the settled queue exceeds
// maxSettledTimers. It only deletes an entry if it still matches the
// generation that settled — a name re-armed since its last fire keeps its
// newer, current entry untouched.
func (tf *TimerFacility) evictSettledTimers() {
	for len(tf.settledTimers) > maxSettledTimers {
		oldest := tf.settledTimers[0]
		tf.settledTimers = tf.settledTimers[1:]
		if tf.activeTimers[oldest.name] == oldest.gen {
			delete(tf.activeTimers, oldest.name)
		}
	}
}

func (tf *TimerFacility) handleIsCurrent(cmd *isCurrentCmd) {
	current := tf.activeTimers[cmd.name] == cmd.timerID
	cmd.resultCh <- current
}

func (tf *TimerFacility) handleFireTimerIfCurrent(cmd *fireTimerIfCurrentCmd) {
	if tf.activeTimers[cmd.name] == cmd.timerID {
		// Timer is still current and has now fired. Keep the entry live
		// (IsCurrent must still report true for this generation right
		// after the completion is delivered) but queue it for eventual
		// eviction so a name that's never re-armed doesn't sit in the map
		// forever.
		tf.settledTimers = append(tf.settledTimers, settledTimer{name: cmd.name, gen: cmd.timerID})
		tf.evictSettledTimers()
		// The timer's own goroutine already called its cancel func on the
		// way to firing (see timerGoroutine's deferred cancel), so the
		// entry no longer needs to be retained; leaving it here leaked one
		// cancelFuncs entry per never-rearmed, never-cancelled name.
		delete(tf.cancelFuncs, cmd.name)
		select {
		case tf.completions <- TimerCompletion{
			Name:     cmd.name,
			TimerID:  cmd.timerID,
			Duration: cmd.duration,
		}:
		case <-tf.ctx.Done():
		}
	}
}

// timerGoroutine runs the timer for a specific name and generation.
// It is ctx/done-guarded so it will exit cleanly when the context is cancelled.
func (tf *TimerFacility) timerGoroutine(name string, timerId int, duration time.Duration, ctx context.Context, cancel context.CancelFunc) {
	defer cancel()

	select {
	case <-tf.after(duration):
		// Send a command to run() to fire the timer if it's still current
		select {
		case tf.commands <- timerCommand{&fireTimerIfCurrentCmd{
			name:     name,
			timerID:  timerId,
			duration: duration,
		}}:
		case <-ctx.Done():
		}

	case <-ctx.Done():
		// Context was cancelled — exit without sending completion
	}
}
