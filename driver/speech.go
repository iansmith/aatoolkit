package driver

// speechQueue serializes TTS clips through one worker so a queued ack and the
// answer that follows it never play over each other. enqueue returns a channel
// that closes when that clip has finished rendering (Speak ignores it;
// SpeakSync waits on it).
type speechQueue struct {
	ch     chan speechItem
	render func(text []byte, voice string, speed float64)
}

type speechItem struct {
	text  []byte
	voice string
	speed float64
	done  chan struct{}
}

func newSpeechQueue(render func(text []byte, voice string, speed float64)) *speechQueue {
	q := &speechQueue{ch: make(chan speechItem, 32), render: render}
	go func() {
		for it := range q.ch {
			q.render(it.text, it.voice, it.speed)
			close(it.done)
		}
	}()
	return q
}

func (q *speechQueue) enqueue(text []byte, voice string, speed float64) <-chan struct{} {
	done := make(chan struct{})
	q.ch <- speechItem{text: text, voice: voice, speed: speed, done: done}
	return done
}

// cancelQueued drops all clips currently in the queue without rendering them.
// Any clip already being rendered runs to completion. done channels of dropped
// clips are closed so SpeakSync callers are not stranded.
func (q *speechQueue) cancelQueued() {
	for {
		select {
		case it := <-q.ch:
			close(it.done)
		default:
			return
		}
	}
}
