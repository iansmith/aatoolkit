package driver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// secretURL is the chat-completions URL at hostport with userinfo and a
// query, both carrying secrets a span must never hold.
func secretURL(hostport string) string {
	return "http://u:pw-secret@" + hostport + "/v1/chat/completions?k=q-secret"
}

// closedHostPort returns the address of a server that is no longer listening,
// so a request to it fails in the transport.
func closedHostPort() string {
	srv := httptest.NewServer(http.NotFoundHandler())
	hostport := srv.Listener.Addr().String()
	srv.Close()
	return hostport
}

// TestModelSpan_NeverRecordsSecretsOrNumbers (DoD f): whatever the outcome --
// success, an upstream status echoing the key and the caller's number, an SSE
// error payload echoing both, a transport failure, a timeout -- no span the
// call ends, the model span or any other, holds the API key, URL userinfo or
// query secrets, an auth header's name or scheme, or an E.164 number. With
// capture on, the same holds for the secrets and headers, using number-free
// fixtures.
func TestModelSpan_NeverRecordsSecretsOrNumbers(t *testing.T) {
	const apiKey = "sk-scan-secret"
	const number = "+15551234567"
	needles := []string{apiKey, "pw-secret", "q-secret", "Bearer", "authorization"}
	// Test-only and unanchored, so it finds a number anywhere in a value.
	e164 := regexp.MustCompile(`\+[1-9]\d{6,14}`)

	type scenario struct {
		name string
		// start returns the tier URL to call, a context for the call, and a
		// cleanup; echo is what the upstream puts in its error replies.
		start func(t *testing.T, echo string) (url string, ctx context.Context, cleanup func())
	}
	serve := func(h http.HandlerFunc) func(t *testing.T, echo string) (string, context.Context, func()) {
		return func(t *testing.T, echo string) (string, context.Context, func()) {
			srv := httptest.NewServer(h)
			return secretURL(srv.Listener.Addr().String()), context.Background(), srv.Close
		}
	}
	scenarios := []scenario{
		{name: "success", start: func(t *testing.T, echo string) (string, context.Context, func()) {
			return serve(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintln(w, textLine("Noted, I will call you back."))
				fmt.Fprintln(w, finishLine("stop"))
				fmt.Fprintln(w, doneLine)
			})(t, echo)
		}},
		{name: "401 echoing the key and number", start: func(t *testing.T, echo string) (string, context.Context, func()) {
			return serve(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "rejected "+r.Header.Get("Authorization")+" for "+echo, http.StatusUnauthorized)
			})(t, echo)
		}},
		{name: "SSE error payload echoing both", start: func(t *testing.T, echo string) (string, context.Context, func()) {
			return serve(func(w http.ResponseWriter, r *http.Request) {
				msg, _ := json.Marshal("rejected " + r.Header.Get("Authorization") + " for " + echo)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintln(w, `data: {"error":{"message":`+string(msg)+`}}`)
			})(t, echo)
		}},
		{name: "transport failure", start: func(t *testing.T, echo string) (string, context.Context, func()) {
			return secretURL(closedHostPort()), context.Background(), func() {}
		}},
		{name: "timeout", start: func(t *testing.T, echo string) (string, context.Context, func()) {
			srv, _ := newScriptedSSEServer(
				sseStep{line: roleOnlyLine},
				sseStep{delay: 5 * time.Second, line: textLine("too late")},
			)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			return secretURL(srv.Listener.Addr().String()), ctx, func() { cancel(); srv.Close() }
		}},
	}

	check := func(t *testing.T, sc scenario, window []byte, echo string, re *regexp.Regexp, wantInput bool) {
		t.Helper()
		url, ctx, cleanup := sc.start(t, echo)
		defer cleanup()
		h, rec := tracedHost(t, url, Tier{Model: "test", MaxTokens: 512, APIKey: apiKey})
		_, _, _ = h.SendStreamContext(ctx, window, "fast", func(string) {})
		span := onlyModelSpan(t, rec)
		for _, hit := range scanSpans(rec.Ended(), needles, re) {
			t.Error(hit)
		}
		if _, ok := attrMap(span)["gen_ai.input.messages"]; ok != wantInput {
			t.Errorf("gen_ai.input.messages present = %v, want %v", ok, wantInput)
		}
	}

	for _, sc := range scenarios {
		t.Run("capture off/"+sc.name, func(t *testing.T) {
			withoutCaptureEnv(t)
			check(t, sc, []byte(`[{"role":"user","content":"Call me back at `+number+` please."}]`), number, e164, false)
		})
		t.Run("capture on/"+sc.name, func(t *testing.T) {
			withCaptureEnv(t, "1")
			check(t, sc, []byte(`[{"role":"user","content":"Call me back please."}]`), "the caller", nil, true)
		})
	}
}

// TestServerAddrPort_StripsUserinfoPathQuery: the address recorded on a span
// is the URL's host alone, and the port is the explicit one or the scheme's.
func TestServerAddrPort_StripsUserinfoPathQuery(t *testing.T) {
	cases := []struct {
		raw      string
		wantAddr string
		wantPort int
	}{
		{"https://u:pw-secret@api.example.invalid/v1/chat/completions?k=q-secret", "api.example.invalid", 443},
		{"http://127.0.0.1:1234/v1/chat/completions", "127.0.0.1", 1234},
		{"http://user@localhost/v1/chat/completions#frag", "localhost", 80},
		{"https://[::1]:8443/v1/chat/completions", "::1", 8443},
		{"ftp://files.example.invalid/x", "files.example.invalid", 0},
		{"http://%zz", "", 0},
		{"", "", 0},
	}
	for _, c := range cases {
		addr, port := serverAddrPort(c.raw)
		if addr != c.wantAddr || port != c.wantPort {
			t.Errorf("serverAddrPort(%q) = %q, %d; want %q, %d", c.raw, addr, port, c.wantAddr, c.wantPort)
		}
	}
}

// TestTraceSource_NoSecretsReachSpanCode (DoD f): trace.go, the file that
// writes spans, is not handed a type that holds a secret or the
// conversation. No parameter, result or receiver of a function in it, no
// field of a struct in it, and no type it declares (an alias included) is or
// holds a Tier (its APIKey), a Host (its tiers, history and prompt), an
// *http.Request, an http.Header or an error (whose text can echo an upstream
// body). "Holds" is followed through every type the package's non-test files
// declare, so a field added to a driver.go struct that trace.go takes, such
// as streamState, counts. trace.go also never names APIKey or the tiers map,
// never calls RecordError, AddEvent or an Error() method, and gives every
// SetStatus the literal "" as its description; it must contain at least one
// SetStatus, so that rule is not vacuous.
//
// This is a tripwire for those routes, not a proof: a string or []byte can
// still carry anything, and the contextWindow and the stream's content reach
// trace.go by design, to be recorded only when capture is on.
// TestModelSpan_NeverRecordsSecretsOrNumbers is the guard on what a span
// actually holds.
func TestTraceSource_NoSecretsReachSpanCode(t *testing.T) {
	fset := token.NewFileSet()
	pos := func(n ast.Node) string { return fset.Position(n.Pos()).String() }

	// Every package-level type the package's non-test files declare, so a
	// type trace.go names can be followed to what it holds.
	files := parsePackageFiles(t, fset)
	f := files["trace.go"]
	decls := map[string]ast.Expr{}
	for _, file := range files {
		for _, d := range file.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
				for _, spec := range gd.Specs {
					ts := spec.(*ast.TypeSpec)
					decls[ts.Name.Name] = ts.Type
				}
			}
		}
	}

	// holds returns the forbidden type that e is or holds, with the path to
	// it, or "" when it reaches none.
	forbidden := map[string]bool{"Tier": true, "Host": true, "error": true}
	var holds func(e ast.Expr, seen map[string]bool) string
	fieldsHold := func(fields *ast.FieldList, seen map[string]bool) string {
		if fields == nil {
			return ""
		}
		for _, field := range fields.List {
			if bad := holds(field.Type, seen); bad != "" {
				return bad
			}
		}
		return ""
	}
	holds = func(e ast.Expr, seen map[string]bool) string {
		switch x := e.(type) {
		case *ast.Ident:
			if forbidden[x.Name] {
				return x.Name
			}
			if def, ok := decls[x.Name]; ok && !seen[x.Name] {
				seen[x.Name] = true
				if bad := holds(def, seen); bad != "" {
					return x.Name + ", which holds " + bad
				}
			}
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "http" && (x.Sel.Name == "Request" || x.Sel.Name == "Header") {
				return "http." + x.Sel.Name
			}
		case *ast.StarExpr:
			return holds(x.X, seen)
		case *ast.ParenExpr:
			return holds(x.X, seen)
		case *ast.Ellipsis:
			return holds(x.Elt, seen)
		case *ast.ArrayType:
			return holds(x.Elt, seen)
		case *ast.ChanType:
			return holds(x.Value, seen)
		case *ast.MapType:
			if bad := holds(x.Key, seen); bad != "" {
				return bad
			}
			return holds(x.Value, seen)
		case *ast.IndexExpr:
			if bad := holds(x.X, seen); bad != "" {
				return bad
			}
			return holds(x.Index, seen)
		case *ast.IndexListExpr:
			if bad := holds(x.X, seen); bad != "" {
				return bad
			}
			for _, ix := range x.Indices {
				if bad := holds(ix, seen); bad != "" {
					return bad
				}
			}
		case *ast.StructType:
			return fieldsHold(x.Fields, seen)
		case *ast.InterfaceType:
			return fieldsHold(x.Methods, seen)
		case *ast.FuncType:
			if bad := fieldsHold(x.Params, seen); bad != "" {
				return bad
			}
			return fieldsHold(x.Results, seen)
		}
		return ""
	}
	checkFields := func(what string, fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, field := range fields.List {
			if bad := holds(field.Type, map[string]bool{}); bad != "" {
				t.Errorf("%s: a %s of type %s", pos(field), what, bad)
			}
		}
	}

	// Control: the walk follows a type declared in another file through a
	// map to a Tier, so a field like Config's would not slip past it.
	if bad := holds(&ast.Ident{Name: "Config"}, map[string]bool{}); bad == "" {
		t.Error("control: Config holds its tiers, but the walk finds no Tier in it")
	}

	setStatus := 0
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			checkFields("receiver", x.Recv)
			checkFields("parameter", x.Type.Params)
			checkFields("result", x.Type.Results)
		case *ast.FuncLit:
			checkFields("parameter", x.Type.Params)
			checkFields("result", x.Type.Results)
		case *ast.StructType:
			checkFields("struct field", x.Fields)
		case *ast.TypeSpec:
			if _, isStruct := x.Type.(*ast.StructType); !isStruct {
				if bad := holds(x.Type, map[string]bool{}); bad != "" {
					t.Errorf("%s: type %s is %s", pos(x), x.Name.Name, bad)
				}
			}
		case *ast.Ident:
			if x.Name == "APIKey" || x.Name == "tiers" {
				t.Errorf("%s: references %s", pos(x), x.Name)
			}
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case sel.Sel.Name == "RecordError" || sel.Sel.Name == "AddEvent":
				t.Errorf("%s: calls %s", pos(x), sel.Sel.Name)
			case sel.Sel.Name == "Error" && len(x.Args) == 0:
				t.Errorf("%s: calls an Error() method", pos(x))
			case sel.Sel.Name == "SetStatus":
				setStatus++
				if len(x.Args) != 2 {
					t.Errorf("%s: SetStatus with %d arguments", pos(x), len(x.Args))
					break
				}
				if lit, ok := x.Args[1].(*ast.BasicLit); !ok || lit.Kind != token.STRING || lit.Value != `""` {
					t.Errorf("%s: SetStatus description is not the literal \"\"", pos(x))
				}
			}
		}
		return true
	})
	if setStatus == 0 {
		t.Error("trace.go calls SetStatus nowhere; the status rule above checked nothing")
	}
}

// TestTraceSource_OnlyTraceGoTouchesSpans (DoD f): trace.go is the only
// non-test file in the package that looks up a tracer or starts, annotates
// or ends a span, so the rules TestTraceSource_NoSecretsReachSpanCode holds
// it to apply to every span the package makes. Every other non-test file
// imports no otel package but go.opentelemetry.io/otel/trace, uses that only
// to name TracerProvider, never selects a field named span, never calls
// tracer or a Tracer method, and never calls Start, End, RecordError,
// AddEvent, SetStatus, SetAttributes or SetName. Each rule must find
// something in trace.go itself, so none of them is vacuous. A call named
// Start or End on something other than a span, were another file to need
// one, would trip it too.
func TestTraceSource_OnlyTraceGoTouchesSpans(t *testing.T) {
	const traceImport = "go.opentelemetry.io/otel/trace"
	spanCalls := map[string]bool{"RecordError": true, "AddEvent": true, "SetStatus": true, "SetAttributes": true, "SetName": true}
	startEnd := map[string]bool{"Start": true, "End": true}

	// spanWrites returns what in path breaks each rule, keyed by the rule.
	spanWrites := func(t *testing.T, path string) map[string][]string {
		t.Helper()
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		hits := map[string][]string{}
		hit := func(rule string, n ast.Node, what string) {
			hits[rule] = append(hits[rule], fmt.Sprintf("%s: %s", fset.Position(n.Pos()), what))
		}
		traceName := ""
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: import path %s: %v", path, imp.Path.Value, err)
			}
			switch {
			case p == traceImport:
				traceName = "trace"
				if imp.Name != nil {
					traceName = imp.Name.Name
				}
			case strings.HasPrefix(p, "go.opentelemetry.io/"):
				hit("otel import", imp, "imports "+p)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if x.Sel.Name == "span" {
					hit("span field", x, "selects .span")
				}
				if id, ok := x.X.(*ast.Ident); ok && traceName != "" && id.Name == traceName && x.Sel.Name != "TracerProvider" {
					hit("trace use", x, "uses "+traceName+"."+x.Sel.Name)
				}
			case *ast.CallExpr:
				switch fn := x.Fun.(type) {
				case *ast.Ident:
					if fn.Name == "tracer" {
						hit("tracer lookup", x, "calls tracer")
					}
				case *ast.SelectorExpr:
					switch {
					case spanCalls[fn.Sel.Name]:
						hit("span call", x, "calls "+fn.Sel.Name)
					case startEnd[fn.Sel.Name]:
						hit("span start or end", x, "calls "+fn.Sel.Name)
					case fn.Sel.Name == "Tracer":
						hit("tracer lookup", x, "calls Tracer")
					}
				}
			}
			return true
		})
		return hits
	}

	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing the package's files: %v", err)
	}
	checked := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") || path == "trace.go" {
			continue
		}
		checked++
		for _, found := range spanWrites(t, path) {
			for _, h := range found {
				t.Error(h)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no non-test file besides trace.go to check")
	}

	control := spanWrites(t, "trace.go")
	for _, rule := range []string{"otel import", "trace use", "span field", "span call", "span start or end", "tracer lookup"} {
		if len(control[rule]) == 0 {
			t.Errorf("control: the %q rule finds nothing in trace.go, so it cannot see a span write anywhere", rule)
		}
	}
}

// parsePackageFiles parses the package's non-test files, keyed by name.
func parsePackageFiles(t *testing.T, fset *token.FileSet) map[string]*ast.File {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing the package's files: %v", err)
	}
	files := map[string]*ast.File{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		files[path] = f
	}
	if files["trace.go"] == nil {
		t.Fatal("trace.go not found")
	}
	return files
}

// TestTraceSource_CaptureVariableReadOnlyThroughGetenv: every lookup of the
// capture variable goes through getenv, so the read count
// TestModelSpan_NoProviderReadsNoCaptureVariable takes covers them all. Its
// name is written once, in trace.go; no other non-test file names it,
// captureMessageContentEnv, captureMessageContent or getenv; and trace.go
// calls nothing in os that reads the environment.
func TestTraceSource_CaptureVariableReadOnlyThroughGetenv(t *testing.T) {
	fset := token.NewFileSet()
	files := parsePackageFiles(t, fset)
	envReads := map[string]bool{"Getenv": true, "LookupEnv": true, "Environ": true}
	names := map[string]bool{"captureMessageContentEnv": true, "captureMessageContent": true, "getenv": true}
	literals := 0
	for path, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if v, err := strconv.Unquote(x.Value); x.Kind == token.STRING && err == nil && v == documentedCaptureEnv {
					literals++
					if path != "trace.go" {
						t.Errorf("%s: names the capture variable", fset.Position(x.Pos()))
					}
				}
			case *ast.Ident:
				if path != "trace.go" && names[x.Name] {
					t.Errorf("%s: references %s", fset.Position(x.Pos()), x.Name)
				}
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && path == "trace.go" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "os" && envReads[sel.Sel.Name] {
						t.Errorf("%s: calls os.%s; read the capture variable through getenv", fset.Position(x.Pos()), sel.Sel.Name)
					}
				}
			}
			return true
		})
	}
	if literals != 1 {
		t.Errorf("the capture variable's name is written %d times in the package's non-test files; want once, in trace.go", literals)
	}
}

// TestModelSpan_ErrorStatusAndType: every failure ends its span with status
// Error, an empty description, and the error.type of its kind, and the error
// returned to the caller is the text it always was.
func TestModelSpan_ErrorStatusAndType(t *testing.T) {
	longBody := strings.Repeat("b", 400)
	var badChunk sseChunk
	decodeErr := json.Unmarshal([]byte(`{not json`), &badChunk)

	type outcome struct {
		url string
		err error
	}
	cases := []struct {
		name string
		// call builds its Host with newHost, runs the request, and returns
		// the URL it used and its error.
		call       func(t *testing.T, newHost func(url string) *Host) outcome
		wantType   string
		wantFinish []string
		// wantErr is the returned error's text: exactly, or as a prefix
		// when the rest comes from the network stack.
		wantErr    func(url string) string
		prefixOnly bool
		wantIs     error
	}{
		{
			name: "caller deadline",
			call: func(t *testing.T, h func(string) *Host) outcome {
				srv, _ := newScriptedSSEServer(sseStep{line: roleOnlyLine}, sseStep{delay: 5 * time.Second, line: textLine("too late")})
				defer srv.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				_, _, err := h(chatURL(srv)).SendStreamContext(ctx, traceWindow, "fast", func(string) {})
				return outcome{chatURL(srv), err}
			},
			wantType: "timeout", wantFinish: []string{"error"},
			wantErr: func(string) string { return "reading SSE stream: " }, prefixOnly: true, wantIs: context.DeadlineExceeded,
		},
		{
			name: "caller cancel",
			call: func(t *testing.T, h func(string) *Host) outcome {
				srv, _ := newScriptedSSEServer(
					sseStep{line: textLine("The quick brown fox jumps over the lazy dog.")},
					sseStep{delay: 5 * time.Second, line: finishLine("stop")},
				)
				defer srv.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				_, _, err := h(chatURL(srv)).SendStreamContext(ctx, traceWindow, "fast", func(string) { cancel() })
				return outcome{chatURL(srv), err}
			},
			wantType: "canceled", wantFinish: []string{"error"},
			wantErr: func(string) string { return "reading SSE stream: " }, prefixOnly: true, wantIs: context.Canceled,
		},
		{
			name: "status 429",
			call: func(t *testing.T, h func(string) *Host) outcome {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, "slow down")
				}))
				defer srv.Close()
				_, _, err := h(chatURL(srv)).SendStream(traceWindow, "fast", func(string) {})
				return outcome{chatURL(srv), err}
			},
			wantType: "429", wantFinish: []string{"error"},
			wantErr: func(url string) string { return fmt.Sprintf("llm %s: status 429 (slow down)", url) },
		},
		{
			name: "status 503 with a long body",
			call: func(t *testing.T, h func(string) *Host) outcome {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusServiceUnavailable)
					fmt.Fprint(w, longBody)
				}))
				defer srv.Close()
				_, _, err := h(chatURL(srv)).SendStream(traceWindow, "fast", func(string) {})
				return outcome{chatURL(srv), err}
			},
			wantType: "503", wantFinish: []string{"error"},
			wantErr: func(url string) string { return fmt.Sprintf("llm %s: status 503 (%s)", url, longBody[:300]) },
		},
		{
			name: "transport failure",
			call: func(t *testing.T, h func(string) *Host) outcome {
				url := "http://" + closedHostPort() + "/v1/chat/completions"
				_, _, err := h(url).SendStream(traceWindow, "fast", func(string) {})
				return outcome{url, err}
			},
			wantType: "transport_error", wantFinish: []string{"error"},
			wantErr: func(url string) string { return "calling " + url + ": " }, prefixOnly: true,
		},
		{
			name: "SSE error payload",
			call: func(t *testing.T, h func(string) *Host) outcome {
				srv, _ := newScriptedSSEServer(sseStep{line: textLine("Hel")}, sseStep{line: `data: {"error":{"message":"overloaded"}}`})
				defer srv.Close()
				_, _, err := h(chatURL(srv)).SendStream(traceWindow, "fast", func(string) {})
				return outcome{chatURL(srv), err}
			},
			wantType: "upstream_error", wantFinish: []string{"error"},
			wantErr: func(string) string { return "llm error: overloaded" },
		},
		{
			name: "SSE error payload after a finish reason",
			call: func(t *testing.T, h func(string) *Host) outcome {
				srv, _ := newScriptedSSEServer(
					sseStep{line: `data: {"choices":[{"delta":{"content":"Hel"},"finish_reason":"length"}]}`},
					sseStep{line: `data: {"error":{"message":"overloaded"}}`},
				)
				defer srv.Close()
				_, _, err := h(chatURL(srv)).SendStream(traceWindow, "fast", func(string) {})
				return outcome{chatURL(srv), err}
			},
			wantType: "upstream_error", wantFinish: []string{"length"},
			wantErr: func(string) string { return "llm error: overloaded" },
		},
		{
			name: "malformed chunk",
			call: func(t *testing.T, h func(string) *Host) outcome {
				srv, _ := newScriptedSSEServer(sseStep{line: `data: {not json`})
				defer srv.Close()
				_, _, err := h(chatURL(srv)).SendStream(traceWindow, "fast", func(string) {})
				return outcome{chatURL(srv), err}
			},
			wantType: "decode_error", wantFinish: []string{"error"},
			wantErr: func(string) string { return "decoding SSE chunk: " + decodeErr.Error() },
		},
		{
			name: "line over the scanner limit",
			call: func(t *testing.T, h func(string) *Host) outcome {
				srv, _ := newScriptedSSEServer(sseStep{line: "data: " + strings.Repeat("x", 300*1024)})
				defer srv.Close()
				_, _, err := h(chatURL(srv)).SendStream(traceWindow, "fast", func(string) {})
				return outcome{chatURL(srv), err}
			},
			wantType: "stream_read_error", wantFinish: []string{"error"},
			wantErr: func(string) string { return "reading SSE stream: " + bufio.ErrTooLong.Error() },
		},
		{
			name: "no content, tool calls only",
			call: func(t *testing.T, h func(string) *Host) outcome {
				srv, _ := newScriptedSSEServer(sseStep{line: finishLine("tool_calls")}, sseStep{line: doneLine})
				defer srv.Close()
				_, _, err := h(chatURL(srv)).SendStream(traceWindow, "fast", func(string) {})
				return outcome{chatURL(srv), err}
			},
			wantType: "empty_response", wantFinish: []string{"tool_calls"},
			wantErr: func(string) string { return "no content in SSE stream (finish_reason tool_calls)" },
		},
		{
			name: "no content at all",
			call: func(t *testing.T, h func(string) *Host) outcome {
				srv, _ := newScriptedSSEServer(sseStep{line: doneLine})
				defer srv.Close()
				_, _, err := h(chatURL(srv)).SendStream(traceWindow, "fast", func(string) {})
				return outcome{chatURL(srv), err}
			},
			wantType: "empty_response", wantFinish: []string{"error"},
			wantErr: func(string) string { return "no content in SSE stream" },
		},
		{
			name: "onSegment panics",
			call: func(t *testing.T, h func(string) *Host) outcome {
				srv, _ := newScriptedSSEServer(okSteps()...)
				defer srv.Close()
				defer func() { _ = recover() }()
				_, _, _ = h(chatURL(srv)).SendStream(traceWindow, "fast", func(string) { panic("onSegment failed") })
				return outcome{}
			},
			wantType: "_OTHER", wantFinish: []string{"error"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withoutCaptureEnv(t)
			var rec *tracetest.SpanRecorder
			newHost := func(url string) *Host {
				h, r := tracedHost(t, url, Tier{Model: "test", MaxTokens: 512})
				rec = r
				return h
			}
			got := tc.call(t, newHost)
			if rec == nil {
				t.Fatal("the case never built its Host")
			}

			span := onlyModelSpan(t, rec)
			requireAttrs(t, span, map[string]any{
				"error.type":                     tc.wantType,
				"gen_ai.response.finish_reasons": tc.wantFinish,
			})
			if st := span.Status(); st.Code != codes.Error || st.Description != "" {
				t.Errorf("status = %v %q, want Error with an empty description", st.Code, st.Description)
			}

			if tc.wantErr == nil {
				return
			}
			if got.err == nil {
				t.Fatalf("no error; want %q", tc.wantErr(got.url))
			}
			want := tc.wantErr(got.url)
			if tc.prefixOnly {
				if !strings.HasPrefix(got.err.Error(), want) {
					t.Errorf("error = %q; want it to start %q", got.err.Error(), want)
				}
			} else if got.err.Error() != want {
				t.Errorf("error = %q; want exactly %q", got.err.Error(), want)
			}
			if tc.wantIs != nil && !errors.Is(got.err, tc.wantIs) {
				t.Errorf("error %q does not wrap %v", got.err, tc.wantIs)
			}
		})
	}
}

// TestSendStreamContext_NoSpanWithoutRequest: a call that fails before its
// span starts -- an unknown tier, or a window that is not valid JSON -- sends
// no request and produces no span. The control call shows the same Host does
// produce one when it sends. A call that sends nothing for a later reason
// still gets a span; see TestSendStreamContext_SpanWhenNothingIsSent.
func TestSendStreamContext_NoSpanWithoutRequest(t *testing.T) {
	srv, reqs := newScriptedSSEServer(okSteps()...)
	defer srv.Close()
	h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512})

	if _, _, err := h.SendStreamContext(context.Background(), traceWindow, "nope", func(string) {}); err == nil {
		t.Error("unknown tier: no error")
	}
	if _, _, err := h.SendStreamContext(context.Background(), []byte(`[{"role":`), "fast", func(string) {}); err == nil {
		t.Error("malformed window: no error")
	}
	if n := len(modelSpans(rec)); n != 0 || reqs.Count() != 0 {
		t.Errorf("after two calls that send nothing: %d model spans, %d requests; want 0 and 0", n, reqs.Count())
	}

	if _, _, err := h.SendStreamContext(context.Background(), traceWindow, "fast", func(string) {}); err != nil {
		t.Fatalf("control call: %v", err)
	}
	if n := len(modelSpans(rec)); n != 1 {
		t.Errorf("control call: %d model spans, want 1", n)
	}
}

// TestSendStreamContext_SpanWhenNothingIsSent: a call whose tier is found and
// whose window encodes gets its span even when no request goes out -- its
// context already done, or its tier URL one no request can be built for --
// and the span records why it failed.
func TestSendStreamContext_SpanWhenNothingIsSent(t *testing.T) {
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name     string
		ctx      context.Context
		badURL   bool
		wantType string
		wantIs   error
	}{
		{name: "context already canceled", ctx: canceled, wantType: "canceled", wantIs: context.Canceled},
		{name: "deadline already passed", ctx: expired, wantType: "timeout", wantIs: context.DeadlineExceeded},
		{name: "URL no request can be built for", ctx: context.Background(), badURL: true, wantType: "transport_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withoutCaptureEnv(t)
			srv, reqs := newScriptedSSEServer(okSteps()...)
			defer srv.Close()
			url := chatURL(srv)
			if tc.badURL {
				url = "http://bad host/v1/chat/completions"
			}
			h, rec := tracedHost(t, url, Tier{Model: "test", MaxTokens: 512})
			_, _, err := h.SendStreamContext(tc.ctx, traceWindow, "fast", func(string) {})
			if err == nil {
				t.Fatal("no error")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error %q does not wrap %v", err, tc.wantIs)
			}
			if n := reqs.Count(); n != 0 {
				t.Errorf("%d requests reached the upstream; want 0", n)
			}
			span := onlyModelSpan(t, rec)
			requireAttrs(t, span, map[string]any{
				"error.type":                     tc.wantType,
				"gen_ai.response.finish_reasons": []string{"error"},
			})
			if st := span.Status(); st.Code != codes.Error || st.Description != "" {
				t.Errorf("status = %v %q, want Error with an empty description", st.Code, st.Description)
			}
		})
	}
}
