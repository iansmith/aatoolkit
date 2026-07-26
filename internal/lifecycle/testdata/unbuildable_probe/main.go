// Package main is a fixture used by source_exec_test.go's build-failure
// test. It is syntactically valid Go — gofmt and go vet do not choke on
// it — but fails to *compile*, referencing a symbol that does not exist, so
// EnsureHealthProbeBuilt's build-failure test can assert on the compiler's
// own diagnostic rather than a bare "it did not build" status.
package main

func main() {
	undefinedProbeFixtureSymbol()
}
