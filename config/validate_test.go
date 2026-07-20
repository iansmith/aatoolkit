package config

import "testing"

func validServer(overrides func(*Server)) Server {
	s := Server{
		Name:    "test-server",
		Type:    TypeMLX,
		Enabled: true,
		Host:    "127.0.0.1",
		Port:    9000,
		Model:   "some-model",
		Health:  Health{Path: "/v1/models"},
	}
	if overrides != nil {
		overrides(&s)
	}
	return s
}

func TestValidate_ReservedServerName(t *testing.T) {
	for _, name := range reservedNames {
		t.Run(name, func(t *testing.T) {
			s := validServer(func(s *Server) { s.Name = name })
			err := Validate(Config{Servers: []Server{s}})
			if err == nil {
				t.Fatalf("expected error for reserved name %q, got nil", name)
			}
		})
	}
}

func TestValidate_DuplicateServerNames(t *testing.T) {
	a := validServer(func(s *Server) { s.Name = "dup"; s.Port = 9001 })
	b := validServer(func(s *Server) { s.Name = "dup"; s.Port = 9002 })
	err := Validate(Config{Servers: []Server{a, b}})
	if err == nil {
		t.Fatal("expected error for duplicate server names, got nil")
	}
}

func TestValidate_PortCollisionAcrossPortAndListens(t *testing.T) {
	a := validServer(func(s *Server) { s.Name = "a"; s.Port = 9000 })
	b := validServer(func(s *Server) {
		s.Name = "b"
		s.Type = TypeExec
		s.Command = "run-me"
		s.Port = 0
		s.Listens = []int{9000}
		s.Health = Health{Port: 9000, Path: "/healthz"}
	})
	err := Validate(Config{Servers: []Server{a, b}})
	if err == nil {
		t.Fatal("expected port collision error, got nil")
	}
}

func TestValidate_PortCollisionWithinListens(t *testing.T) {
	a := validServer(func(s *Server) {
		s.Name = "a"
		s.Type = TypeExec
		s.Command = "x"
		s.Port = 0
		s.Listens = []int{80, 443}
		s.Health = Health{Port: 80, Path: "/healthz"}
	})
	b := validServer(func(s *Server) {
		s.Name = "b"
		s.Type = TypeExec
		s.Command = "y"
		s.Port = 0
		s.Listens = []int{443, 8443}
		s.Health = Health{Port: 443, Path: "/healthz"}
	})
	err := Validate(Config{Servers: []Server{a, b}})
	if err == nil {
		t.Fatal("expected port collision error for overlapping listens, got nil")
	}
}

func TestValidate_RequiredFieldsPerType(t *testing.T) {
	cases := []struct {
		name string
		s    Server
	}{
		{"mlx missing model", validServer(func(s *Server) { s.Model = "" })},
		{"python missing venv", validServer(func(s *Server) {
			s.Type = TypePython
			s.Model = ""
			s.Entry = "entry"
			s.Packages = []string{"pkg"}
		})},
		{"python missing entry", validServer(func(s *Server) {
			s.Type = TypePython
			s.Model = ""
			s.Venv = ".venv"
			s.Packages = []string{"pkg"}
		})},
		{"python missing packages", validServer(func(s *Server) {
			s.Type = TypePython
			s.Model = ""
			s.Venv = ".venv"
			s.Entry = "entry"
		})},
		{"source missing build", validServer(func(s *Server) {
			s.Type = TypeSource
			s.Model = ""
			s.Binary = "build/bin"
		})},
		{"source missing binary", validServer(func(s *Server) {
			s.Type = TypeSource
			s.Model = ""
			s.Build = "go build"
		})},
		{"exec missing command", validServer(func(s *Server) {
			s.Type = TypeExec
			s.Model = ""
		})},
		{"unknown type", validServer(func(s *Server) {
			s.Type = ServerType("bogus")
			s.Model = ""
		})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(Config{Servers: []Server{c.s}})
			if err == nil {
				t.Fatalf("expected validation error, got nil")
			}
		})
	}
}

func TestValidate_RequiresAtLeastOnePort(t *testing.T) {
	s := validServer(func(s *Server) {
		s.Port = 0
		s.Health = Health{Port: 9000, Path: "/v1/models"}
	})
	err := Validate(Config{Servers: []Server{s}})
	if err == nil {
		t.Fatal("expected error when server declares no port and no listens, got nil")
	}
}

func TestValidate_HealthPathRequired(t *testing.T) {
	s := validServer(func(s *Server) { s.Health = Health{} })
	err := Validate(Config{Servers: []Server{s}})
	if err == nil {
		t.Fatal("expected error for missing health.path, got nil")
	}
}

func TestValidate_HealthPortMustBeInDeclaredSet(t *testing.T) {
	s := validServer(func(s *Server) {
		s.Port = 9000
		s.Health = Health{Port: 9999, Path: "/v1/models"}
	})
	err := Validate(Config{Servers: []Server{s}})
	if err == nil {
		t.Fatal("expected error when health.port is not in the server's port set, got nil")
	}
}

func TestValidate_HealthPortDefaultsToServerPort(t *testing.T) {
	s := validServer(func(s *Server) {
		s.Port = 9000
		s.Health = Health{Path: "/v1/models"} // no explicit port
	})
	if err := Validate(Config{Servers: []Server{s}}); err != nil {
		t.Fatalf("expected no error when health.port defaults to server port, got %v", err)
	}
}

func TestValidate_HealthPortInListensSet(t *testing.T) {
	s := validServer(func(s *Server) {
		s.Type = TypeExec
		s.Model = ""
		s.Command = "caddy"
		s.Port = 0
		s.Listens = []int{80, 443, 2019}
		s.Health = Health{Port: 2019, Path: "/config/"}
	})
	if err := Validate(Config{Servers: []Server{s}}); err != nil {
		t.Fatalf("expected no error for health.port in listens set, got %v", err)
	}
}

func TestValidate_ValidConfigPasses(t *testing.T) {
	s := validServer(nil)
	if err := Validate(Config{Servers: []Server{s}}); err != nil {
		t.Fatalf("expected valid config to pass, got %v", err)
	}
}

func TestValidate_EmptyServerNameRejected(t *testing.T) {
	s := validServer(func(s *Server) { s.Name = "" })
	err := Validate(Config{Servers: []Server{s}})
	if err == nil {
		t.Fatal("expected error for empty server name, got nil")
	}
}

func TestValidate_SourceDirRequiresGoBuildCommand(t *testing.T) {
	s := validServer(func(s *Server) {
		s.Type = TypeSource
		s.Model = ""
		s.Build = "make build"
		s.Binary = "build/bin"
		s.Dir = "~/other-project"
	})
	err := Validate(Config{Servers: []Server{s}})
	if err == nil {
		t.Fatal("expected error when 'dir' is set on a non-'go build' command, got nil")
	}
}

func TestValidate_SourceDirWithGoBuildCommandPasses(t *testing.T) {
	s := validServer(func(s *Server) {
		s.Type = TypeSource
		s.Model = ""
		s.Build = "go build -o build/bin ."
		s.Binary = "build/bin"
		s.Dir = "~/other-project"
	})
	if err := Validate(Config{Servers: []Server{s}}); err != nil {
		t.Fatalf("expected 'dir' with a 'go build' command to pass, got %v", err)
	}
}
