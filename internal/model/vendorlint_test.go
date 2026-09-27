package model_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// vendorSDKPrefixes lists import path prefixes that count as a "vendor
// SDK" for the purposes of AGENTS.md's rule: "Vendor SDKs may be imported
// only inside internal/model/<adapter>/. A lint test enforces it."
//
// Empty for S1 on purpose (design §4.1: local is the only adapter and has
// no vendor SDK). The placeholder entry below is a canary proving the
// mechanism itself works — it matches nothing real, so it never fires,
// but a typo that made it match everything would turn up immediately in
// this repo's own tree. S5 adds the real entries here (e.g. a Gemini or a
// hosted-decisions SDK) when those adapters land.
var vendorSDKPrefixes = []string{
	"github.com/tokencanopy/abusekit/internal/model/__vendor_sdk_placeholder__",
}

// TestNoVendorSDKOutsideAdapterPackages walks every .go file in the
// module and fails if a file outside internal/model/<adapter>/ imports a
// path matching vendorSDKPrefixes. "outside internal/model/<adapter>/"
// means: not a direct child package directory of internal/model (so
// internal/model itself, and every non-model package, are checked;
// internal/model/local, internal/model/fake, internal/model/gemini, etc.
// are exempt).
func TestNoVendorSDKOutsideAdapterPackages(t *testing.T) {
	root := moduleRoot(t)

	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base != "." && strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if isAdapterPackagePath(rel) {
			return nil
		}

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("parse %s: %v", rel, err)
			return nil
		}
		for _, imp := range f.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			for _, prefix := range vendorSDKPrefixes {
				if strings.HasPrefix(importPath, prefix) {
					violations = append(violations, rel+" imports vendor SDK path "+importPath)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("vendor SDK imported outside internal/model/<adapter>/:\n%s", strings.Join(violations, "\n"))
	}
}

// isAdapterPackagePath reports whether rel (a module-root-relative .go
// file path) lives directly under internal/model/<adapter>/, i.e. has at
// least one path segment after "internal/model".
func isAdapterPackagePath(rel string) bool {
	const prefix = "internal/model/"
	if !strings.HasPrefix(rel, prefix) {
		return false
	}
	rest := strings.TrimPrefix(rel, prefix)
	return strings.Contains(rest, "/") // has a subdirectory segment before the filename
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod above %s", dir)
		}
		dir = parent
	}
}
