package gonnx

import (
	"os"
	"slices"
	"testing"
)

// testDataDir is the vendored subset of the ONNX backend node tests. It is
// tracked in git so `go test` works on a cold clone with no network; see
// README.md for how to add a case directory from a `make test_data` refresh.
const testDataDir = "./test_data"

// TestVendoredTestDataCoversReferencedCases asserts that every case name
// ops_test.go refers to — whether it runs (expectedTests) or is skipped
// (ignoredTests) — has its data vendored. Without this, adding a name to
// either list without also vendoring its directory fails deep inside TestOps
// with an opaque PathError from os.Open, or silently skips the case.
//
// The model.onnx check matters as much as the directory one: a half-copied
// case directory would otherwise pass here and still produce the very
// PathError this test exists to prevent.
func TestVendoredTestDataCoversReferencedCases(t *testing.T) {
	for _, name := range slices.Concat(expectedTests, ignoredTests) {
		caseDir := testDataDir + "/" + name

		info, err := os.Stat(caseDir)
		if err != nil {
			t.Errorf("%s is referenced by ops_test.go but has no vendored data: %v", name, err)
			continue
		}

		if !info.IsDir() {
			t.Errorf("%s is not a directory", caseDir)
			continue
		}

		if _, err := os.Stat(caseDir + "/model.onnx"); err != nil {
			t.Errorf("%s is vendored but incomplete: %v", name, err)
		}
	}
}
