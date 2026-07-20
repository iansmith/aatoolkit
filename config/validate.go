package config

import (
	"fmt"
	"slices"
	"strings"
)

// reservedNames are the REPL verbs a server name must not collide with —
// see design/aa-server-status.md §2. A collision would make "aa-server-status>
// <name>" ambiguous between "run this verb" and "show this server."
var reservedNames = []string{
	"status", "up", "down", "dead", "build", "logs", "details",
	"help", "quit", "exit", "bye",
}

// Validate runs all structural checks on a fully merged config. Every
// failure here is a hard error — config problems abort the whole program
// at read time (design/aa-server-status.md §6.5).
func Validate(cfg Config) error {
	seenNames := make(map[string]bool, len(cfg.Servers))
	portOwners := make(map[int]string)

	for _, s := range cfg.Servers {
		if err := validateName(s, seenNames); err != nil {
			return err
		}
		if err := validateType(s); err != nil {
			return err
		}
		if err := validatePorts(s, portOwners); err != nil {
			return err
		}
		if err := validateHealth(s); err != nil {
			return err
		}
	}
	return nil
}

func validateName(s Server, seen map[string]bool) error {
	if s.Name == "" {
		return fmt.Errorf("server: name is required")
	}
	if slices.Contains(reservedNames, s.Name) {
		return fmt.Errorf("server %q: name collides with a reserved REPL verb", s.Name)
	}
	if seen[s.Name] {
		return fmt.Errorf("server %q: duplicate name", s.Name)
	}
	seen[s.Name] = true
	return nil
}

// serverPortSet returns the {port} ∪ listens set declared for a server.
func serverPortSet(s Server) []int {
	ports := make([]int, 0, len(s.Listens)+1)
	if s.Port != 0 {
		ports = append(ports, s.Port)
	}
	ports = append(ports, s.Listens...)
	return ports
}

func validatePorts(s Server, owners map[int]string) error {
	ports := serverPortSet(s)
	if len(ports) == 0 {
		return fmt.Errorf("server %q: needs at least one port (port or listens)", s.Name)
	}
	for _, p := range ports {
		if owner, ok := owners[p]; ok {
			return fmt.Errorf("server %q: port %d collides with server %q", s.Name, p, owner)
		}
		owners[p] = s.Name
	}
	return nil
}

func validateType(s Server) error {
	switch s.Type {
	case TypeMLX:
		if s.Model == "" {
			return fmt.Errorf("server %q: type mlx requires 'model'", s.Name)
		}
	case TypePython:
		if s.Venv == "" || s.Entry == "" || len(s.Packages) == 0 {
			return fmt.Errorf("server %q: type python requires 'venv', 'entry', and 'packages'", s.Name)
		}
	case TypeSource:
		if s.Build == "" || s.Binary == "" {
			return fmt.Errorf("server %q: type source requires 'build' and 'binary'", s.Name)
		}
		if s.Dir != "" && !strings.HasPrefix(strings.TrimSpace(s.Build), "go ") {
			return fmt.Errorf("server %q: 'dir' requires a 'go build' command (got %q) — -C is a go-specific flag", s.Name, s.Build)
		}
	case TypeExec:
		if s.Command == "" {
			return fmt.Errorf("server %q: type exec requires 'command'", s.Name)
		}
	default:
		return fmt.Errorf("server %q: unknown type %q (must be mlx, python, exec, or source)", s.Name, s.Type)
	}
	return nil
}

func validateHealth(s Server) error {
	if s.Health.Path == "" {
		return fmt.Errorf("server %q: health.path is required", s.Name)
	}
	if s.Health.Port == 0 {
		// Defaults to the server's own port — must have one to default to.
		if s.Port == 0 {
			return fmt.Errorf("server %q: health.port is required when the server has no scalar 'port' to default to", s.Name)
		}
		return nil
	}
	ports := serverPortSet(s)
	if !slices.Contains(ports, s.Health.Port) {
		return fmt.Errorf("server %q: health.port %d is not one of the server's declared ports %v", s.Name, s.Health.Port, ports)
	}
	return nil
}
