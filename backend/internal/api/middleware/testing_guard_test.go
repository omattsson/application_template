package middleware

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// usesTestIdentity reports whether the Go source calls or references
// SetTestIdentity, as middleware.SetTestIdentity or (inside this package) the
// bare name.
func usesTestIdentity(t *testing.T, filename, src string) bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), filename, src, parser.SkipObjectResolution)
	require.NoError(t, err, filename)
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if x.Sel.Name == "SetTestIdentity" {
				found = true
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "SetTestIdentity" {
				found = true
			}
		}
		return !found
	})
	return found
}

func TestUsesTestIdentityDetectsTheCalls(t *testing.T) {
	t.Parallel()
	assert.True(t, usesTestIdentity(t, "a.go", "package a\nfunc f() { middleware.SetTestIdentity(c, \"u\", \"admin\", \"\") }"))
	assert.True(t, usesTestIdentity(t, "a.go", "package a\nfunc f() { SetTestIdentity(c, \"u\", \"admin\", \"\") }"))
	assert.True(t, usesTestIdentity(t, "a.go", "package a\nvar f = middleware.SetTestIdentity"))
	assert.False(t, usesTestIdentity(t, "a.go", "package a\n// SetTestIdentity is only mentioned here\nfunc f() {}"))
}

// SetTestIdentity puts an identity on the context without checking anything:
// a route that called it would trust its caller. It is exported because the
// handler tests (another package) need it, and Go has no way to export a
// symbol to test code only. So this test keeps it out of the production
// code: no non-test file of the backend may use it, which also keeps it out
// of the production binary (the linker drops a function nothing calls; verify
// with `go build ./api && go tool nm <binary>`). Only testing.go defines it.
func TestSetTestIdentityIsNotUsedByProductionCode(t *testing.T) {
	t.Parallel()

	// The test runs in backend/internal/api/middleware; the walk covers backend/.
	root, err := filepath.Abs("../../..")
	require.NoError(t, err)
	definition := filepath.Join(root, "internal", "api", "middleware", "testing.go")
	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "vendor" || name == "tmp" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || path == definition {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checked++
		rel, _ := filepath.Rel(root, path)
		assert.False(t, usesTestIdentity(t, path, string(src)),
			"%s uses SetTestIdentity, which is for tests only: it sets an identity without checking anything", rel)
		return nil
	})
	require.NoError(t, err)
	assert.Greater(t, checked, 50, "the walk found the backend sources")
}
