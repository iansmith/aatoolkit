package telephony

import (
	"context"
	"errors"
	"log"
	"sync"
)

var ErrUnknownSession = errors.New("telephony: reply router: unknown session")

type ReplySink struct {
	responseIn ResponseInput
	router     *ReplyRouter
	sessionID  string
}

type ReplyRouter struct {
	mu    sync.RWMutex
	sinks map[string]*ReplySink
}

func NewReplyRouter() *ReplyRouter {
	return &ReplyRouter{
		sinks: make(map[string]*ReplySink),
	}
}

func (r *ReplyRouter) Register(sessionID string, responseIn ResponseInput) *ReplySink {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.sinks[sessionID]; exists {
		log.Printf("telephony: reply router: WARN duplicate registration for session %s, replacing", sessionID)
	}

	sink := &ReplySink{responseIn: responseIn, router: r, sessionID: sessionID}
	r.sinks[sessionID] = sink
	return sink
}

// Deregister removes sessionID's route unconditionally, by key alone. If two
// registrations have raced for the same sessionID (mirrors the STTRouter
// scenario this type is modeled on: two live calls sharing a CallSID), this
// can remove a newer, still-live registration rather than the caller's own —
// callers that hold the *ReplySink Register returned should prefer Close,
// which deregisters only if it is still the registered sink for its session.
func (r *ReplyRouter) Deregister(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sinks, sessionID)
}

// Close deregisters sink, but only if it is still the sink registered under
// its session ID -- unlike Deregister(sessionID), it cannot remove a
// different, newer registration that has since replaced it under the same
// key. This is the identity check STTRouter.Deregister(sessionID, route)
// performs; ReplySink carries the equivalent identity here since Deregister's
// signature (frozen by the Phase 0 test contract) cannot take it.
func (s *ReplySink) Close() {
	if s == nil || s.router == nil {
		return
	}
	s.router.mu.Lock()
	defer s.router.mu.Unlock()
	if cur, ok := s.router.sinks[s.sessionID]; ok && cur == s {
		delete(s.router.sinks, s.sessionID)
	}
}

// Route delivers an ok ResponseEvent carrying frames to sessionID's response
// input.
func (r *ReplyRouter) Route(ctx context.Context, sessionID string, frames [][]byte) error {
	r.mu.RLock()
	sink, ok := r.sinks[sessionID]
	r.mu.RUnlock()

	if !ok {
		log.Printf("telephony: reply router: dropping frames for unregistered session %s", sessionID)
		return ErrUnknownSession
	}

	if err := sink.responseIn.Send(ctx, ResponseEvent{OK: true, Frames: frames}); err != nil {
		log.Printf("telephony: reply router: WARN send failed for session %s: %v", sessionID, err)
		return err
	}
	return nil
}

// Fail delivers a failed ResponseEvent to sessionID's response input.
func (r *ReplyRouter) Fail(ctx context.Context, sessionID string) error {
	r.mu.RLock()
	sink, ok := r.sinks[sessionID]
	r.mu.RUnlock()

	if !ok {
		log.Printf("telephony: reply router: dropping fail for unregistered session %s", sessionID)
		return ErrUnknownSession
	}

	if err := sink.responseIn.Send(ctx, ResponseEvent{OK: false}); err != nil {
		log.Printf("telephony: reply router: WARN send failed for session %s: %v", sessionID, err)
		return err
	}
	return nil
}
