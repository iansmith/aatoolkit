package telephony

import (
	"context"
	"errors"
	"log"
	"sync"
)

var ErrUnknownSession = errors.New("telephony: reply router: unknown session")

type ReplySink struct {
	dataOut TwilioDataPlaneOutput
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

func (r *ReplyRouter) Register(sessionID string, dataOut TwilioDataPlaneOutput) *ReplySink {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.sinks[sessionID]; exists {
		log.Printf("telephony: reply router: WARN duplicate registration for session %s, replacing", sessionID)
	}

	sink := &ReplySink{dataOut: dataOut}
	r.sinks[sessionID] = sink
	return sink
}

func (r *ReplyRouter) Deregister(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sinks, sessionID)
}

func (r *ReplyRouter) Route(ctx context.Context, sessionID string, frames [][]byte) error {
	r.mu.RLock()
	sink, ok := r.sinks[sessionID]
	r.mu.RUnlock()

	if !ok {
		log.Printf("telephony: reply router: dropping frames for unregistered session %s", sessionID)
		return ErrUnknownSession
	}

	for _, frame := range frames {
		if err := sink.dataOut.Send(ctx, frame); err != nil {
			log.Printf("telephony: reply router: WARN send failed for session %s: %v", sessionID, err)
			return err
		}
	}
	return nil
}
