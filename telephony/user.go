package telephony

// User is the interface for user context injected into the LLM conversation.
// It provides a single method to retrieve the user-description block that is
// placed before the conversation history when sending to the LLM.
type User interface {
	// Context returns the user-description block to place before the conversation
	// when we send to the LLM. It may return an empty string if no user context
	// is currently available.
	Context() string
}
