package config

import (
	"slices"
	"strings"
	"testing"
)

// --- decoding the exec form ---

// The exec form is argv, never a shell line: the supervisor runs the program
// directly (mirroring lifecycle.ExecCommand's command+args), so the argument
// with a space in it survives instead of being split into two.
func TestHealth_UnmarshalTOML_ExecForm(t *testing.T) {
	h := decodeHealth(t, `health = { exec = ["pg_isready", "-h", "127.0.0.1", "-d", "my db"] }`)
	want := []string{"pg_isready", "-h", "127.0.0.1", "-d", "my db"}
	if !slices.Equal(h.Exec, want) {
		t.Errorf("Exec = %q, want %q", h.Exec, want)
	}
	if h.Path != "" {
		t.Errorf("Path = %q, want empty for an exec-form health check", h.Path)
	}
}

func TestHealth_UnmarshalTOML_ExecForm_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"non-string element", `health = { exec = ["pg_isready", 5432] }`},
		{"empty argv", `health = { exec = [] }`},
		{"not an array", `health = { exec = "pg_isready" }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := decodeHealthErr(t, tc.src); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// --- Declared: the one predicate for "this server has a readiness check" ---

func TestHealth_Declared(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    Health
		want bool
	}{
		{"zero value", Health{}, false},
		{"http form", Health{Path: "/healthz"}, true},
		{"exec form", Health{Exec: []string{"pg_isready"}}, true},
		{"host and port only", Health{Host: "127.0.0.1", Port: 9000}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.h.Declared(); got != tc.want {
				t.Errorf("Declared() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- validation: exactly one form ---

// execServer is validServer's datastore counterpart: the same fixture, turned
// into the thing this ticket exists for — a server that does not speak HTTP and
// so declares its readiness as a command.
func execServer(overrides func(*Server)) Server {
	return validServer(func(s *Server) {
		s.Name = "datastore"
		s.Type = TypeExec
		s.Model = ""
		s.Command = "docker"
		s.Args = []string{"compose", "up"}
		s.Port = 5432
		s.Health = Health{Exec: []string{"db-probe", "--port", "5432"}}
		if overrides != nil {
			overrides(s)
		}
	})
}

// The symptom in the ticket: a server whose readiness cannot be expressed as an
// HTTP GET must be declarable at all.
func TestValidate_HealthExecFormPasses(t *testing.T) {
	if err := Validate(Config{Servers: []Server{execServer(nil)}}); err != nil {
		t.Fatalf("expected exec-form health to validate, got %v", err)
	}
}

// The same thing through the path an operator actually takes — strict decode
// plus Validate, from a real file. This is the reproduction of the reported
// failure: `server "datastore": health.path is required` on load.
func TestLoad_HealthExecFormLoadsAndValidates(t *testing.T) {
	cfg, err := Load("testdata/health-exec.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(cfg.Servers))
	}

	http := cfg.Servers[0]
	if http.Health.Path != "/v1/models" || len(http.Health.Exec) != 0 {
		t.Errorf("HTTP server health decoded as %+v, want path-only", http.Health)
	}

	datastore := cfg.Servers[1]
	if got := strings.Join(datastore.Health.Exec, " "); got != "/usr/local/bin/db-probe --host 127.0.0.1 --port 5432" {
		t.Errorf("exec argv decoded as %q", got)
	}
	if datastore.Health.Path != "" {
		t.Errorf("Path = %q, want empty for the exec form", datastore.Health.Path)
	}
}

// Exactly one form, and nothing alongside the exec form that cannot reach it.
// Each case names the words the error has to carry — an error that rejects the
// config without saying which field is wrong just moves the guessing.
func TestValidate_HealthExactlyOneForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		// why states the failure the case is fencing, so a future reader
		// changing one of these messages knows what it was for.
		why      string
		server   Server
		mustName []string
	}{
		{
			name: "neither form",
			why:  "health stays mandatory, and the error names both alternatives so the next person hitting the original symptom is not left guessing that exec exists",
			server: validServer(func(s *Server) {
				s.Health = Health{}
			}),
			mustName: []string{"path", "exec"},
		},
		{
			name: "both forms",
			why:  "ambiguous — rejected rather than resolved by precedence, which would silently ignore one of the two the operator wrote",
			server: execServer(func(s *Server) {
				s.Health = Health{Path: "/healthz", Exec: []string{"db-probe"}}
			}),
			mustName: []string{"path", "exec"},
		},
		{
			name: "exec with host",
			why:  "host cannot reach a command; accepting it discards a value written on purpose",
			server: execServer(func(s *Server) {
				s.Health.Host = "127.0.0.1"
			}),
			mustName: []string{"host"},
		},
		{
			name: "exec with port",
			why:  "port cannot reach a command, same reason as host",
			server: execServer(func(s *Server) {
				s.Health.Port = 5432
			}),
			mustName: []string{"port"},
		},
		{
			name: "exec with warm",
			why:  "a warm-up is an HTTP POST to the server's own port, which a server declaring an exec check does not answer — a launch-time failure caught at read time instead",
			server: execServer(func(s *Server) {
				s.Warm = Warm{Method: "POST", Path: "/warm"}
			}),
			mustName: []string{"warm", "exec"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(Config{Servers: []Server{tc.server}})
			if err == nil {
				t.Fatalf("expected error — %s", tc.why)
			}
			for _, want := range tc.mustName {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q must name %q", err.Error(), want)
				}
			}
		})
	}
}
