package config

// ServerByName returns the [[server]] entry with the given name, and
// whether one was found. It lets a caller look up a named server without
// hand-rolling a loop over Config.Servers.
func (c Config) ServerByName(name string) (Server, bool) {
	for _, s := range c.Servers {
		if s.Name == name {
			return s, true
		}
	}
	return Server{}, false
}

// WebhookPort resolves a server's webhook/streams port and whether one is
// declared. By the convention already documented in aa-server-status.toml
// (e.g. "listens = [9730, 9740] # twilio-cli↔HTTP + Twilio"), a server's
// primary listen is Listens[0] and its webhook/streams port is Listens[1];
// servers declaring fewer than two listens have no webhook port.
func (s Server) WebhookPort() (int, bool) {
	if len(s.Listens) < 2 {
		return 0, false
	}
	return s.Listens[1], true
}
