package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture bank is shared with the txtar scripts and, later, the shell
// lifecycle tests, so it lives at tests/fixtures rather than in a testdata
// directory belonging to one package.
func fixtureDir(t *testing.T, parts ...string) string {
	t.Helper()
	return filepath.Join(append([]string{"..", "..", "tests", "fixtures"}, parts...)...)
}

// TestFixtureBankInstalls is what keeps the fixtures honest. A fixture that has
// drifted out of validity fails every test that uses it, in a way that looks
// like the code broke — so check the fixtures themselves, separately, first.
func TestFixtureBankInstalls(t *testing.T) {
	root := fixtureDir(t, "local-stack", "bank")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	seen := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			m, err := Load(filepath.Join(root, e.Name()))
			if err != nil {
				t.Fatalf("fixture should be valid: %v", err)
			}
			if err := m.CheckSchemaVersion(); err != nil {
				t.Errorf("CheckSchemaVersion: %v", err)
			}
			// Both fixtures must install into a lab on the current stack.
			if err := m.CheckMinStack("v1.9.0"); err != nil {
				t.Errorf("CheckMinStack: %v", err)
			}
			// name must equal the directory name and every file basename.
			if m.Name != e.Name() {
				t.Errorf("name %q != directory %q", m.Name, e.Name())
			}
		})
		seen++
	}
	if seen == 0 {
		t.Fatal("no fixture entries found; this test would be vacuous")
	}
}

// The two valid fixtures deliberately straddle a stack version, so a test can
// pin a lab between them and watch exactly one be refused. If that stops being
// true the fixtures have lost the property they were built for.
func TestFixtureBankStraddlesAStackVersion(t *testing.T) {
	root := fixtureDir(t, "local-stack", "bank")

	older, err := Load(filepath.Join(root, "acme"))
	if err != nil {
		t.Fatal(err)
	}
	newer, err := Load(filepath.Join(root, "widget"))
	if err != nil {
		t.Fatal(err)
	}

	const between = "v1.8.0"
	if err := older.CheckMinStack(between); err != nil {
		t.Errorf("acme should install on %s: %v", between, err)
	}
	if err := newer.CheckMinStack(between); err == nil {
		t.Errorf("widget should be refused on %s", between)
	}
}

// A manifest carrying no exposed routes carries no cred-gateway config, and
// that is not an oversight in the fixture — it is the shape a
// proxy-injection-only provider takes.
func TestFixtureWithNothingExposedShipsNoGatewayConfig(t *testing.T) {
	dir := fixtureDir(t, "local-stack", "bank", "widget")
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range m.BrokerRoutes {
		if r.IsExposed() {
			t.Fatalf("fixture is meant to expose nothing, but %s is exposed", r.Path)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "cred-gateway")); err == nil {
		t.Error("an entry that exposes nothing must ship no cred-gateway config")
	}
}

// TestRefusedFixtures pins WHY each refused fixture is refused. A fixture that
// starts failing for a different reason than intended still passes a bare
// "must error" assertion while no longer testing what it was written for.
func TestRefusedFixtures(t *testing.T) {
	cases := []struct {
		dir string
		// wantDecodeErr: rejected while reading the file at all.
		wantDecodeErr string
		// wantCheckErr: shape is fine; a check refuses it.
		wantCheckErr string
	}{
		{dir: "unknown-field", wantDecodeErr: "audit_mode"},
		{dir: "missing-exposed", wantDecodeErr: "exposed"},
		{dir: "future-schema", wantCheckErr: "99"},
		{dir: "stack-too-new", wantCheckErr: "99.0.0"},
	}

	for _, c := range cases {
		t.Run(c.dir, func(t *testing.T) {
			m, err := Load(fixtureDir(t, "refused", c.dir))

			if c.wantDecodeErr != "" {
				if err == nil {
					t.Fatalf("expected the manifest to be rejected while reading it")
				}
				if !strings.Contains(err.Error(), c.wantDecodeErr) {
					t.Errorf("error should mention %q, got: %v", c.wantDecodeErr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("this fixture's shape is valid; it is the checks that must refuse it: %v", err)
			}
			err = m.CheckSchemaVersion()
			if err == nil {
				err = m.CheckMinStack("v1.9.0")
			}
			if err == nil {
				t.Fatal("expected a check to refuse this manifest")
			}
			if !strings.Contains(err.Error(), c.wantCheckErr) {
				t.Errorf("error should mention %q, got: %v", c.wantCheckErr, err)
			}
		})
	}
}

// The leaky fixture is the one a manifest reader can never catch: every field
// is correct, and the failure is between the manifest and a file beside it.
// This test asserts the trap is still armed — that the manifest still looks
// clean and the config still contradicts it — so that whoever writes the
// cross-check has a fixture that actually fails it.
func TestLeakyFixtureIsStillArmed(t *testing.T) {
	dir := fixtureDir(t, "refused", "leaky-conf")

	m, err := Load(dir)
	if err != nil {
		t.Fatalf("the manifest is supposed to be valid — that is the point: %v", err)
	}

	var unexposed []string
	for _, r := range m.BrokerRoutes {
		if !r.IsExposed() {
			unexposed = append(unexposed, r.Path)
		}
	}
	if len(unexposed) == 0 {
		t.Fatal("fixture no longer marks any route exposed:false, so it cannot leak one")
	}

	conf, err := os.ReadFile(filepath.Join(dir, "cred-gateway", "leaky.conf"))
	if err != nil {
		t.Fatal(err)
	}
	leaked := false
	for _, path := range unexposed {
		if strings.Contains(string(conf), path) {
			leaked = true
		}
	}
	if !leaked {
		t.Fatal("fixture's config no longer whitelists an unexposed route; it has stopped being a trap")
	}
}
