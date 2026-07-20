package main

import "testing"

// --- edge / boundary cases ---

func TestParseCommand_BareEnterIsStatus(t *testing.T) {
	cmd, err := ParseCommand("")
	if err != nil {
		t.Fatalf("ParseCommand(\"\"): unexpected error: %v", err)
	}
	if cmd.Verb != VerbStatus || cmd.Target != "" {
		t.Fatalf("bare Enter should parse as bare status, got %+v", cmd)
	}
}

func TestParseCommand_WhitespaceOnlyIsStatus(t *testing.T) {
	cmd, err := ParseCommand("   \t  ")
	if err != nil {
		t.Fatalf("ParseCommand(whitespace): unexpected error: %v", err)
	}
	if cmd.Verb != VerbStatus || cmd.Target != "" {
		t.Fatalf("whitespace-only input should parse as bare status, got %+v", cmd)
	}
}

func TestParseCommand_ExtraWhitespaceIsTolerated(t *testing.T) {
	cmd, err := ParseCommand("   status   ")
	if err != nil {
		t.Fatalf("ParseCommand: unexpected error: %v", err)
	}
	if cmd.Verb != VerbStatus || cmd.Target != "" {
		t.Fatalf("padded 'status' should parse as bare status, got %+v", cmd)
	}
}

func TestParseCommand_PerServerVerbWithExtraSpaces(t *testing.T) {
	cmd, err := ParseCommand("myserver    up")
	if err != nil {
		t.Fatalf("ParseCommand: unexpected error: %v", err)
	}
	if cmd.Verb != VerbUp || cmd.Target != "myserver" {
		t.Fatalf("expected up on myserver, got %+v", cmd)
	}
}

// --- error / rejection cases ---

func TestParseCommand_LogsWithoutNameIsError(t *testing.T) {
	_, err := ParseCommand("logs")
	if err == nil {
		t.Fatal("ParseCommand(\"logs\") with no server name should error loudly")
	}
}

// A single unrecognized token is syntactically identical to a bare server
// name (ParseCommand has no server list to check against) — it parses as
// a per-server status request. Whether "frobnicate" names a real server
// is checked at dispatch time, against the engine's known servers; that's
// where "unknown input" actually becomes a loud error for this case. See
// TestRun_UnknownServerNameIsLoudError in repl_test.go for that seam.
func TestParseCommand_SingleUnrecognizedTokenIsBareNameStatus(t *testing.T) {
	cmd, err := ParseCommand("frobnicate")
	if err != nil {
		t.Fatalf("ParseCommand(\"frobnicate\"): unexpected error: %v", err)
	}
	if cmd.Verb != VerbStatus || cmd.Target != "frobnicate" {
		t.Fatalf("expected bare-name status for unrecognized single token, got %+v", cmd)
	}
}

func TestParseCommand_UnknownPerServerVerbIsError(t *testing.T) {
	_, err := ParseCommand("myserver launch")
	if err == nil {
		t.Fatal("ParseCommand(\"myserver launch\") should error — 'launch' is not up|down|build")
	}
}

func TestParseCommand_TooManyTokensIsError(t *testing.T) {
	_, err := ParseCommand("myserver up now")
	if err == nil {
		t.Fatal("ParseCommand with trailing garbage after a valid per-server verb should error")
	}
}

func TestParseCommand_LogsWithExtraTokensIsError(t *testing.T) {
	_, err := ParseCommand("logs myserver extra")
	if err == nil {
		t.Fatal("ParseCommand(\"logs myserver extra\") should error — logs takes exactly one name")
	}
}

// --- cross-feature interaction (all documented verb forms) ---

func TestParseCommand_GlobalVerbs(t *testing.T) {
	cases := map[string]Verb{
		"status": VerbStatus,
		"up":     VerbUp,
		"down":   VerbDown,
		"dead":   VerbDead,
		"build":  VerbBuild,
		"help":   VerbHelp,
		"quit":   VerbQuit,
		"exit":   VerbExit,
		"bye":    VerbBye,
	}
	for input, want := range cases {
		cmd, err := ParseCommand(input)
		if err != nil {
			t.Errorf("ParseCommand(%q): unexpected error: %v", input, err)
			continue
		}
		if cmd.Verb != want || cmd.Target != "" {
			t.Errorf("ParseCommand(%q) = %+v, want verb=%v target=\"\"", input, cmd, want)
		}
	}
}

func TestParseCommand_LogsWithName(t *testing.T) {
	cmd, err := ParseCommand("logs myserver")
	if err != nil {
		t.Fatalf("ParseCommand: unexpected error: %v", err)
	}
	if cmd.Verb != VerbLogs || cmd.Target != "myserver" {
		t.Fatalf("expected logs on myserver, got %+v", cmd)
	}
}

func TestParseCommand_BareNameIsPerServerStatus(t *testing.T) {
	cmd, err := ParseCommand("myserver")
	if err != nil {
		t.Fatalf("ParseCommand: unexpected error: %v", err)
	}
	if cmd.Verb != VerbStatus || cmd.Target != "myserver" {
		t.Fatalf("bare name should parse as per-server status, got %+v", cmd)
	}
}

func TestParseCommand_PerServerVerbs(t *testing.T) {
	cases := map[string]Verb{
		"myserver up":    VerbUp,
		"myserver down":  VerbDown,
		"myserver build": VerbBuild,
	}
	for input, want := range cases {
		cmd, err := ParseCommand(input)
		if err != nil {
			t.Errorf("ParseCommand(%q): unexpected error: %v", input, err)
			continue
		}
		if cmd.Verb != want || cmd.Target != "myserver" {
			t.Errorf("ParseCommand(%q) = %+v, want verb=%v target=myserver", input, cmd, want)
		}
	}
}

func TestParseCommand_ServerNamedLikeAGlobalVerbWithPerServerSuffix(t *testing.T) {
	// A server can legitimately be named e.g. "up" only if config validation
	// forbids reserved-verb names (it does, elsewhere) — but the PARSER
	// itself must not special-case this: "up up" is ambiguous only if the
	// parser conflates "first token is a verb" with "first token is a name."
	// Since names colliding with verbs are rejected at config-validation
	// time, the parser's job is simply: first token decides verb-vs-name
	// by matching the fixed verb set, consistently, with no double meaning.
	cmd, err := ParseCommand("up down")
	if err != nil {
		t.Fatalf("ParseCommand(\"up down\"): unexpected error: %v", err)
	}
	if cmd.Verb != VerbDown || cmd.Target != "up" {
		t.Fatalf("expected 'up' treated as a server name with verb down, got %+v", cmd)
	}
}

// --- happy path ---

func TestParseCommand_Status(t *testing.T) {
	cmd, err := ParseCommand("status")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.Verb != VerbStatus {
		t.Fatalf("expected VerbStatus, got %+v", cmd)
	}
}

// --- kill verb ---

func TestParseCommand_KillWithPID(t *testing.T) {
	cmd, err := ParseCommand("kill 12345")
	if err != nil {
		t.Fatalf("ParseCommand(\"kill 12345\"): unexpected error: %v", err)
	}
	if cmd.Verb != VerbKill || cmd.Target != "12345" {
		t.Fatalf("expected VerbKill with target \"12345\", got %+v", cmd)
	}
}

func TestParseCommand_KillWithoutPIDIsError(t *testing.T) {
	_, err := ParseCommand("kill")
	if err == nil {
		t.Fatal("ParseCommand(\"kill\") with no PID should error")
	}
}

func TestParseCommand_KillWithNonNumericPIDIsError(t *testing.T) {
	_, err := ParseCommand("kill abc")
	if err == nil {
		t.Fatal("ParseCommand(\"kill abc\") with non-numeric PID should error")
	}
}

func TestParseCommand_KillWithZeroPIDIsError(t *testing.T) {
	_, err := ParseCommand("kill 0")
	if err == nil {
		t.Fatal("ParseCommand(\"kill 0\") should error — PID must be positive")
	}
}

func TestParseCommand_KillWithNegativePIDIsError(t *testing.T) {
	_, err := ParseCommand("kill -1")
	if err == nil {
		t.Fatal("ParseCommand(\"kill -1\") should error — PID must be positive")
	}
}

func TestParseCommand_KillWithExtraTokensIsError(t *testing.T) {
	_, err := ParseCommand("kill 12345 extra")
	if err == nil {
		t.Fatal("ParseCommand(\"kill 12345 extra\") should error — kill takes exactly one PID")
	}
}

// --- command verb ---

func TestParseCommand_CommandWithName(t *testing.T) {
	cmd, err := ParseCommand("command chat-llm")
	if err != nil {
		t.Fatalf("ParseCommand(\"command chat-llm\"): unexpected error: %v", err)
	}
	if cmd.Verb != VerbCommand || cmd.Target != "chat-llm" {
		t.Fatalf("expected VerbCommand with target \"chat-llm\", got %+v", cmd)
	}
}

func TestParseCommand_CommandWithoutNameIsError(t *testing.T) {
	_, err := ParseCommand("command")
	if err == nil {
		t.Fatal("ParseCommand(\"command\") with no server name should error")
	}
}

func TestParseCommand_CommandWithExtraTokensIsError(t *testing.T) {
	_, err := ParseCommand("command chat-llm extra")
	if err == nil {
		t.Fatal("ParseCommand(\"command chat-llm extra\") should error — command takes exactly one name")
	}
}

// --- view verb ---

func TestParseCommand_ViewWithName(t *testing.T) {
	cmd, err := ParseCommand("chat-llm view")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.Verb != VerbView || cmd.Target != "chat-llm" || cmd.Modifier != "" {
		t.Fatalf("expected VerbView target=chat-llm modifier=\"\", got %+v", cmd)
	}
}

func TestParseCommand_ViewWithNowrap(t *testing.T) {
	cmd, err := ParseCommand("server view nowrap")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.Verb != VerbView || cmd.Target != "server" || cmd.Modifier != "nowrap" {
		t.Fatalf("expected VerbView target=server modifier=nowrap, got %+v", cmd)
	}
}

func TestParseCommand_ViewWithInvalidModifierIsError(t *testing.T) {
	_, err := ParseCommand("server view something")
	if err == nil {
		t.Fatal("expected error for invalid view modifier, got nil")
	}
}

func TestParseCommand_ViewWithExtraTokensIsError(t *testing.T) {
	_, err := ParseCommand("server view nowrap extra")
	if err == nil {
		t.Fatal("expected error for too many tokens after view nowrap")
	}
}
