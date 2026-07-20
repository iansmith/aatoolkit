package telephony

import (
	"context"
	"log"
	"sync"
)

// sttRouteDepth is each per-session result channel's buffer size. A live
// session drains results promptly from its select loop; a handful of slots
// absorbs scheduling jitter without letting a dead session's results pile up
// unboundedly.
const sttRouteDepth = 16

// STTRouter fans the single sttService result stream out to per-session
// channels, keyed by SessionID. It exists because sttOut is process-global
// (one sttService for all calls): sessions receiving from it directly race
// each other, so a result can be delivered to -- and dropped by -- the wrong
// session, or sit orphaned in the shared buffer until an unrelated later
// call drains it (both observed live; see handleSTTResult's SessionID
// guard, which with the router in place becomes a true invariant check
// rather than load-bearing routing).
//
// Lifecycle: Register a session's channel before its call starts consuming
// results, Deregister after the call ends. Results for a SessionID with no
// registered route are logged and dropped -- with per-session routing that
// is the correct fate for a result whose call has already hung up.
type STTRouter struct {
	results STTOutput

	mu     sync.Mutex
	routes map[string]*BufferedChan[STTResult]
}

// NewSTTRouter returns a router that reads from results (the channel
// sttService writes to). Call Run to start dispatching.
func NewSTTRouter(results STTOutput) *STTRouter {
	return &STTRouter{
		results: results,
		routes:  make(map[string]*BufferedChan[STTResult]),
	}
}

// Register creates, stores, and returns the per-session result channel for
// sessionID -- the value to wire into the session via WithSTTOutput.
// Registering a SessionID that is already registered replaces the old route
// (logged loudly: it means two live calls share a CallSID, which Twilio
// guarantees against -- but twilio-cli is a stand-in that mints its own).
//
// The returned route is also the handle to Deregister with: see there.
func (r *STTRouter) Register(sessionID string) STTOutput {
	route := NewBufferedChan[STTResult](sttRouteDepth)
	r.mu.Lock()
	if _, exists := r.routes[sessionID]; exists {
		log.Printf("telephony: stt router: WARN duplicate registration for session %s, replacing", sessionID)
	}
	r.routes[sessionID] = route
	r.mu.Unlock()
	return route
}

// Deregister removes sessionID's route, but only if route is still the one
// registered under it. Results arriving after this are logged and dropped.
// Deregistering an unknown SessionID, or one whose route has since been
// replaced, is a no-op.
//
// The identity check is what makes duplicate SessionIDs merely bad rather
// than silently destructive. Two calls sharing a CallSID means the second
// Register replaces the first's route; when the first call then ends, a
// key-only delete would remove the *second*, live call's route, and every
// result for it from then on would be dropped as "unregistered" -- a call
// going deaf for the rest of its life because an unrelated one hung up.
// Comparing identity means an ending call can only ever remove its own route.
func (r *STTRouter) Deregister(sessionID string, route STTOutput) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.routes[sessionID]; ok && STTOutput(cur) == route {
		delete(r.routes, sessionID)
	}
}

// Run reads results until ctx is cancelled or the results channel errors,
// dispatching each to its session's registered route. The send is
// non-blocking: a full route (a session that stopped draining) drops the
// result with a loud log rather than stalling every other session's
// delivery.
func (r *STTRouter) Run(ctx context.Context) {
	for {
		res, err := r.results.Recv(ctx)
		if err != nil {
			return
		}
		r.mu.Lock()
		route, ok := r.routes[res.SessionID]
		r.mu.Unlock()
		if !ok {
			log.Printf("telephony: stt router: dropping %s result for unregistered session %s (request %d): call already ended",
				res.Kind, res.SessionID, res.RequestID)
			continue
		}
		select {
		case route.ch <- res:
		default:
			log.Printf("telephony: stt router: WARN route full for session %s, dropping %s result (request %d)",
				res.SessionID, res.Kind, res.RequestID)
		}
	}
}
