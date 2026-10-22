package bonnie

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Inventory root sources, including untracked files and constructors outside options.go,
// so a new Option cannot silently disappear from the all-options reference.
func TestDocumentationOptionCoverage(t *testing.T) {
	t.Parallel()
	text, err := os.ReadFile("www/pages/reference/options.md")
	if err != nil {
		t.Fatal(err)
	}
	files, err := exec.CommandContext(t.Context(), "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.go").Output()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for name := range strings.SplitSeq(string(files), "\x00") {
		if name == "" || strings.Contains(name, "/") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() || fn.Type.Results == nil {
				continue
			}
			for _, result := range fn.Type.Results.List {
				ident, ok := result.Type.(*ast.Ident)
				if !ok || ident.Name != "Option" {
					continue
				}
				count++
				if !strings.Contains(string(text), "`"+fn.Name.Name+"(") {
					t.Errorf("%s: %s missing from options reference", name, fn.Name.Name)
				}
			}
		}
	}
	if count == 0 {
		t.Fatal("no Option constructors found")
	}
}
