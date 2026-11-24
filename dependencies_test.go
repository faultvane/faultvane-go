package faultvane_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/faultvane/faultvane-go"

var policyPackages = []string{"circuitbreaker", "timeout", "retry", "ratelimit", "loadshed", "bulkhead", "concurrencylimit"}

func moduleImports(t *testing.T, includeTests bool) map[string][]string {
	t.Helper()
	imports := map[string][]string{}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != "." && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			if _, statErr := os.Stat(filepath.Join(path, "go.mod")); path != "." && statErr == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || (!includeTests && strings.HasSuffix(path, "_test.go")) {
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			imports[filepath.ToSlash(filepath.Dir(path))] = append(imports[filepath.ToSlash(filepath.Dir(path))], imported)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return imports
}

func TestDependencies(t *testing.T) {
	t.Run("TK-5 DS-7 the core module imports only the standard library", func(t *testing.T) {
		imports := moduleImports(t, true)
		if len(imports["circuitbreaker"]) == 0 {
			t.Fatal("the walk found no imports in circuitbreaker")
		}
		for dir, list := range imports {
			for _, imported := range list {
				first, _, _ := strings.Cut(imported, "/")
				if strings.Contains(first, ".") && imported != modulePath && !strings.HasPrefix(imported, modulePath+"/") {
					t.Errorf("%s imports %s", dir, imported)
				}
			}
		}
	})

	t.Run("TK-6 the core module has no requirements", func(t *testing.T) {
		content, err := os.ReadFile("go.mod")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(content), "module "+modulePath+"\n") || strings.Contains(string(content), "require") {
			t.Fatalf("go.mod = %q", content)
		}
	})

	t.Run("TK-10 a policy imports no network package", func(t *testing.T) {
		imports := moduleImports(t, false)
		for _, policy := range policyPackages {
			for _, imported := range imports[policy] {
				if imported == "net" || strings.HasPrefix(imported, "net/") {
					t.Errorf("%s imports %s", policy, imported)
				}
			}
		}
	})

	t.Run("TK-1 a policy does not import a different policy", func(t *testing.T) {
		imports := moduleImports(t, false)
		for _, policy := range policyPackages {
			for _, other := range policyPackages {
				if other != policy && slices.Contains(imports[policy], modulePath+"/"+other) {
					t.Errorf("%s imports %s", policy, other)
				}
			}
			if !slices.Contains(imports[policy], modulePath) {
				t.Errorf("%s does not import the core package", policy)
			}
		}
	})
}
