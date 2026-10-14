package smsfake

// HoldRecording takes the lock the capture server records a message under and
// returns its release. It is for the tests, which otherwise cannot stop a POST
// between its arrival and its recording to see which comes first: the record
// or the wake-up on Notify.
func (c *CaptureServer) HoldRecording() (release func()) {
	c.mu.Lock()
	return c.mu.Unlock
}
