// Package invariants holds structural tests over this repo's own source.
//
// They are tests rather than review notes because the properties they check are
// the ones that fail silently: nothing breaks at the moment they are violated,
// and the cost only arrives later, in another repo.
package invariants

import (
	"bufio"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestNoBankEntryKnowledgeInSource is the guard on the design constraint that
// shapes this whole repo:
//
//	The bank is data. sal is a generic installer over it. There is no
//	per-provider code in this repo, ever.
//
// Adding a provider must be someone dropping bank/<name>/ into the stack repo,
// with zero changes here. The moment this fails, someone has taught the CLI
// about a specific provider and the two repos are coupled again — which is
// precisely what splitting them was meant to make structurally impossible.
//
// It is an AST walk rather than a grep so that a name in a comment, in a
// doc string, or in a fixture path does not read as a violation, and so that a
// name genuinely used as a value cannot hide behind formatting.
func TestNoBankEntryKnowledgeInSource(t *testing.T) {
	root := moduleRoot(t)
	names := readFixtureSet(t, "bank-names.txt")
	allowed := readFixtureSet(t, "allowed-literals.txt")

	// A fixture that has quietly become empty would make this test pass
	// forever while checking nothing. That is a worse position than not
	// having the test at all, so fail loudly instead.
	if len(names) == 0 {
		t.Fatal("testdata/bank-names.txt is empty; this test would be vacuous")
	}

	var violations []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// testdata is where naming a real entry is legitimate: fixtures
			// have to describe the real world to be worth anything.
			case "testdata", ".git", "vendor", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}

		// Import paths carry this module's own name, which contains a string
		// that is also a bank entry name. An import path cannot teach the CLI
		// about a provider, so it is not what this test is looking for.
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
			if allowed[strings.ToLower(value)] {
				return true
			}
			for _, tok := range tokenize(value) {
				if names[tok] {
					violations = append(violations, strings.Join([]string{
						rel + ":" + strconv.Itoa(fset.Position(lit.Pos()).Line),
						"names bank entry " + strconv.Quote(tok),
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
			"this repo must contain no per-provider code; %d literal(s) name a bank entry:\n  %s\n\n"+
				"If the literal is genuinely not provider knowledge (a URL, say), add the whole\n"+
				"literal to internal/invariants/testdata/allowed-literals.txt — deliberately a\n"+
				"visible diff, so weakening this check cannot happen quietly.",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// A detector that has stopped detecting passes silently forever, which is the
// same failure mode the test above exists to prevent. So check it against the
// shapes a real violation would actually take, built from the fixture rather
// than written out here — a bank entry name in this file would be a violation.
func TestDetectorFindsRealisticLiterals(t *testing.T) {
	names := readFixtureSet(t, "bank-names.txt")
	if len(names) == 0 {
		t.Fatal("testdata/bank-names.txt is empty")
	}

	for name := range names {
		shapes := []string{
			name,                        // case "x":
			"/" + name + "/token",       // a broker route
			name + ".py",                // an addon filename
			"bank/" + name,              // an entry path
			"NNN_" + name + ".py",       // an assigned slot
			strings.ToUpper(name) + "_", // an env prefix
		}
		for _, s := range shapes {
			found := false
			for _, tok := range tokenize(s) {
				if names[tok] {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("detector missed %q", s)
			}
		}
	}

	// And it must not fire on everything, or it is just a failing test.
	for _, s := range []string{"", "docker compose", "127.0.0.1", "observer", "/etc/nginx/gateway.d"} {
		for _, tok := range tokenize(s) {
			if names[tok] {
				t.Errorf("detector false-positived on %q via token %q", s, tok)
			}
		}
	}
}

var tokenSplit = regexp.MustCompile(`[^a-z0-9]+`)

// tokenize lowercases and splits on anything that is not alphanumeric, so that
// "/x/token", "x.py" and "bank/x" all surface x. Substring matching would be
// unusable here: several bank names appear inside unrelated hostnames.
func tokenize(s string) []string {
	return tokenSplit.Split(strings.ToLower(s), -1)
}

func readFixtureSet(t *testing.T, name string) map[string]bool {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	defer f.Close()

	set := map[string]bool{}
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		set[strings.ToLower(line)] = true
	}
	if err := scan.Err(); err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return set
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the test's working directory")
		}
		dir = parent
	}
}
