// Package main is a fixture used by source_test.go to build a real,
// deterministic binary for staleness-detection tests. Its content
// (the marker string) intentionally differs from buildable_v2's so the two
// compiled binaries hash differently.
package main

func main() {
	println("server-lifecycle-test-marker-v1")
}
