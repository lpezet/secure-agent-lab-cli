package invariants

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestNoCredentialShapeKnowledgeInSource guards the sibling of the
// no-per-provider-code rule:
//
//	sal never inspects a credential's CONTENTS to choose a DESTINATION.
//
// The destination always comes from the manifest — `file` on the secret the
// bank entry declares. A literal like "sk-ant-oat01-" in this repo's source
// means something here has started deciding where a credential goes by looking
// at what it says, which is per-vendor knowledge one level further in than a
// bank entry name, and fails worse: guessing wrong writes a credential into
// the file the broker reads for a DIFFERENT one, which is silent at install
// and surfaces much later as a rejected request nobody traces back.
//
// This is deliberately narrower than "sal never reads a credential". It does
// read one — to write it to a file — and internal/secrets stats what was typed
// to ask whether the operator meant a path. Neither picks a destination, and
// the second is confirmed by a human before anything is copied.
//
// Substring matching rather than the token matching TestNoBankEntryKnowledge
// uses: a credential prefix is a prefix, so the question is whether a literal
// contains it at all.
func TestNoCredentialShapeKnowledgeInSource(t *testing.T) {
	root := moduleRoot(t)
	shapes := readFixtureLines(t, "credential-shapes.txt")
	if len(shapes) == 0 {
		t.Fatal("testdata/credential-shapes.txt is empty; this test would be vacuous")
	}

	var violations []string
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// Same exemption as the sibling test, for the same reason: naming
			// a real credential shape is legitimate in a fixture — the fixture
			// bank's own values look like credentials on purpose — and nowhere
			// else in this repo.
			case "testdata", ".git", "vendor", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		// Import paths carry this module's own name; an import path cannot
		// teach the CLI what a credential looks like.
		imports := map[*ast.BasicLit]bool{}
		for _, spec := range file.Imports {
			imports[spec.Path] = true
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}

		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || imports[lit] {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, shape := range shapes {
				if strings.Contains(strings.ToLower(value), strings.ToLower(shape)) {
					violations = append(violations, strings.Join([]string{
						rel + ":" + strconv.Itoa(fset.Position(lit.Pos()).Line),
						"contains credential shape " + strconv.Quote(shape),
						"in literal " + lit.Value,
					}, " "))
					break
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf(
			"sal must not know what any vendor's credentials look like; %d literal(s) do:\n  %s\n\n"+
				"A provider with two credential kinds declares two secrets in its manifest,\n"+
				"each with its own file and env var. Which one the operator meant is settled\n"+
				"by which prompt they answered — not by what the value starts with.",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// A detector that has stopped detecting passes silently forever.
func TestCredentialShapeDetectorFires(t *testing.T) {
	shapes := readFixtureLines(t, "credential-shapes.txt")

	contains := func(value string) bool {
		for _, shape := range shapes {
			if strings.Contains(strings.ToLower(value), strings.ToLower(shape)) {
				return true
			}
		}
		return false
	}

	// The shapes a real violation would take: a prefix compared against, a
	// switch arm, a message naming one.
	for _, shape := range shapes {
		for _, form := range []string{
			shape,
			shape + "oat01-",
			"expected a key starting with " + shape,
		} {
			if !contains(form) {
				t.Errorf("detector missed %q", form)
			}
		}
	}

	// And it must not fire on ordinary source, or it is just a failing test.
	for _, s := range []string{
		"", "docker compose", "127.0.0.1", "/etc/nginx/gateway.d",
		"read the credential from a file instead of prompting",
		"secrets", "0600", "schema_version",
	} {
		if contains(s) {
			t.Errorf("detector false-positived on %q", s)
		}
	}
}

// readFixtureLines is readFixtureSet's ordered sibling. These are substrings
// rather than tokens, so a set keyed on the lowercased line would lose nothing
// — but the error message reads better naming the shape as written.
func readFixtureLines(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	defer f.Close()

	var lines []string
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	if err := scan.Err(); err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return lines
}
