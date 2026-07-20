// Package interp is the generic mechanism for loading Go "policy" source at
// runtime: read a directory of .go files, compile them as a named package under a
// fresh yaegi interpreter, inject a set of host symbols the policy may import, and
// evaluate an expression (typically a function symbol) that the caller type-asserts
// and calls. A fresh interpreter per Load means a reload drops the previous code
// cleanly.
//
// It bakes in no entrypoint contract: the package name, the injected symbols, and
// the evaluated expression are all parameters, so any program can load its own
// interpreted policy against its own host interface.
package interp

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing/fstest"

	yaegi "github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// Inject maps an import path (in yaegi's "importpath/pkgname" form, e.g.
// "example.com/host/host") to the exported symbols made available to interpreted
// code under that import. Only the symbols listed are visible; typically an
// interface type via reflect.ValueOf((*Iface)(nil)).
type Inject map[string]map[string]reflect.Value

// Load reads the flat *.go files in dir, maps them as package pkgName under a fresh
// yaegi interpreter, makes stdlib plus inject available, imports the package, and
// evaluates evalExpr — returning the resulting value for the caller to type-assert.
// evalExpr is typically "pkgName.Symbol".
func Load(dir, pkgName string, inject Inject, evalExpr string) (reflect.Value, error) {
	sfs, err := loadFS(dir, pkgName)
	if err != nil {
		return reflect.Value{}, err
	}
	i := yaegi.New(yaegi.Options{GoPath: "", SourcecodeFilesystem: sfs})
	if err := i.Use(stdlib.Symbols); err != nil {
		return reflect.Value{}, err
	}
	if err := i.Use(yaegi.Exports(inject)); err != nil {
		return reflect.Value{}, err
	}
	if _, err := i.Eval(fmt.Sprintf("import %q", pkgName)); err != nil {
		return reflect.Value{}, fmt.Errorf("compiling %s: %w", pkgName, err)
	}
	v, err := i.Eval(evalExpr)
	if err != nil {
		return reflect.Value{}, fmt.Errorf("resolving %s: %w", evalExpr, err)
	}
	return v, nil
}

// loadFS maps flat dir/*.go under src/<pkgName>/ so yaegi's package resolver
// (GoPath "") finds `import "<pkgName>"`.
func loadFS(dir, pkgName string) (fs.FS, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	m := fstest.MapFS{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		m["src/"+pkgName+"/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("no .go files found in %s", dir)
	}
	return m, nil
}
