package manifest

import (
	"strings"
	"testing"
)

// Fixtures use invented entry names. Real bank entry names must not appear in
// this repo's Go source — see internal/invariants.
const validManifest = `{
  "schema_version": 1,
  "name": "acme",
  "summary": "One line, for listings",
  "min_stack": "1.7.0",
  "load_band": "provider",
  "hosts": ["api.acme.example"],
  "secrets": [
    {"env": "ACME_KEY_PATH", "file": "acme.pem", "prompt": "ACME private key", "multiline": true}
  ],
  "config": [
    {"env": "ACME_APP_ID", "prompt": "App ID", "help": "https://acme.example/apps"}
  ],
  "broker_routes": [
    {"path": "/acme/token", "exposed": false},
    {"path": "/acme/credential", "exposed": true}
  ],
  "lab_env": {"ACME_URL": "http://cred-gateway/acme/credential"},
  "lab_setup": "lab/setup.sh"
}`

func decode(t *testing.T, s string) (*Manifest, error) {
	t.Helper()
	return Decode(strings.NewReader(s))
}

func TestDecodeValid(t *testing.T) {
	m, err := decode(t, validManifest)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.SchemaVersion == nil || *m.SchemaVersion != 1 {
		t.Errorf("schema_version = %v, want 1", m.SchemaVersion)
	}
	if m.LoadBand != BandProvider {
		t.Errorf("load_band = %q", m.LoadBand)
	}
	if len(m.BrokerRoutes) != 2 {
		t.Fatalf("broker_routes = %d, want 2", len(m.BrokerRoutes))
	}
	if m.BrokerRoutes[0].IsExposed() {
		t.Error("first route should not be exposed")
	}
	if !m.BrokerRoutes[1].IsExposed() {
		t.Error("second route should be exposed")
	}
	if err := m.CheckSchemaVersion(); err != nil {
		t.Errorf("CheckSchemaVersion: %v", err)
	}
}

// The schema is additionalProperties:false. DisallowUnknownFields is that
// check, and this test is what says so out loud.
func TestDecodeRejectsUnknownField(t *testing.T) {
	in := strings.Replace(validManifest, `"name": "acme",`, `"name": "acme", "extra_control": true,`, 1)
	_, err := decode(t, in)
	if err == nil {
		t.Fatal("want error for unknown field")
	}
	if !strings.Contains(err.Error(), "extra_control") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

func TestDecodeRejectsTrailingContent(t *testing.T) {
	if _, err := decode(t, validManifest+"\n{}"); err == nil {
		t.Fatal("want error for trailing JSON")
	}
}

// The four failure modes the stack's own lint verifies, checked from this side.
func TestSchemaVersionFailureModes(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		in := strings.Replace(validManifest, `"schema_version": 1,`, ``, 1)
		if _, err := decode(t, in); err == nil {
			t.Fatal("want error when schema_version is absent")
		}
	})

	t.Run("string", func(t *testing.T) {
		in := strings.Replace(validManifest, `"schema_version": 1,`, `"schema_version": "1",`, 1)
		if _, err := decode(t, in); err == nil {
			t.Fatal(`want error for "1" as a string`)
		}
	})

	t.Run("fractional", func(t *testing.T) {
		in := strings.Replace(validManifest, `"schema_version": 1,`, `"schema_version": 1.5,`, 1)
		if _, err := decode(t, in); err == nil {
			t.Fatal("want error for 1.5")
		}
	})

	// The one that matters most: a manifest from the future is refused, not
	// attempted, because what it would skip may be a control.
	t.Run("future", func(t *testing.T) {
		in := strings.Replace(validManifest, `"schema_version": 1,`, `"schema_version": 99,`, 1)
		m, err := decode(t, in)
		if err != nil {
			t.Fatalf("shape is still valid, so decode should pass: %v", err)
		}
		err = m.CheckSchemaVersion()
		if err == nil {
			t.Fatal("want refusal for a future schema_version")
		}
		if !strings.Contains(err.Error(), "99") {
			t.Errorf("error should name the offending generation, got: %v", err)
		}
	})
}

func TestValidateRequiresExposedOnEveryRoute(t *testing.T) {
	in := strings.Replace(validManifest, `{"path": "/acme/token", "exposed": false},`, `{"path": "/acme/token"},`, 1)
	_, err := decode(t, in)
	if err == nil {
		t.Fatal("want error when a route omits exposed")
	}
	if !strings.Contains(err.Error(), "exposed") {
		t.Errorf("error should say which field, got: %v", err)
	}
}

func TestValidatePatterns(t *testing.T) {
	cases := map[string]struct{ from, to string }{
		"name":     {`"name": "acme"`, `"name": "Acme"`},
		"minStack": {`"min_stack": "1.7.0"`, `"min_stack": "v1.7.0"`},
		"band":     {`"load_band": "provider"`, `"load_band": "middle"`},
		"secretEnv": {
			`"env": "ACME_KEY_PATH"`,
			`"env": "acme_key_path"`,
		},
		"routePath": {`"path": "/acme/token"`, `"path": "acme/token"`},
		"labEnvKey": {`"ACME_URL"`, `"acme_url"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := strings.Replace(validManifest, c.from, c.to, 1)
			if in == validManifest {
				t.Fatalf("fixture no longer contains %q", c.from)
			}
			if _, err := decode(t, in); err == nil {
				t.Errorf("want error for %s", name)
			}
		})
	}
}

func TestCheckMinStack(t *testing.T) {
	m, err := decode(t, validManifest) // min_stack 1.7.0
	if err != nil {
		t.Fatal(err)
	}

	// Both spellings of the deployment's version must behave identically.
	for _, ok := range []string{"1.7.0", "v1.7.0", "1.9.0", "v1.9.0", "2.0.0"} {
		if err := m.CheckMinStack(ok); err != nil {
			t.Errorf("CheckMinStack(%q) = %v, want nil", ok, err)
		}
	}
	for _, tooOld := range []string{"1.6.0", "v1.6.0", "1.1.0", "0.9.9"} {
		if err := m.CheckMinStack(tooOld); err == nil {
			t.Errorf("CheckMinStack(%q) = nil, want refusal", tooOld)
		}
	}
	if err := m.CheckMinStack("latest"); err == nil {
		t.Error("an unparseable deployment version must be an error, not a pass")
	}
}

func TestBandSlotRanges(t *testing.T) {
	cases := []struct {
		band   Band
		lo, hi int
	}{
		{BandPolicy, 0, 9},
		{BandProvider, 10, 899},
		{BandPost, 900, 999},
	}
	for _, c := range cases {
		lo, hi, ok := c.band.SlotRange()
		if !ok || lo != c.lo || hi != c.hi {
			t.Errorf("%s.SlotRange() = %d, %d, %v", c.band, lo, hi, ok)
		}
	}
	if _, _, ok := Band("nope").SlotRange(); ok {
		t.Error("unknown band should not resolve to a range")
	}
}

// Two declarations must never write to one name. The schema cannot express
// uniqueness across array items, so Validate does.
//
// This is what makes `sal secrets set <provider> <file>` total: the selector is
// an EXACT match on `file`, so uniqueness is what removes every tie there is to
// break. Without it, one of two credentials sharing a file would be
// unaddressable, and whichever was prompted for second would silently overwrite
// the first.
func TestValidateRejectsDuplicateNames(t *testing.T) {
	cases := map[string]struct {
		insert string
		want   string
	}{
		"duplicate file": {
			`{"env": "ACME_KEY_PATH", "file": "acme.pem", "prompt": "A"},
             {"env": "ACME_OTHER_PATH", "file": "acme.pem", "prompt": "B"}`,
			"file",
		},
		// Case-insensitively, because a manifest that is unambiguous on Linux
		// and collides on a case-insensitive filesystem must fail in both.
		"duplicate file, different case": {
			`{"env": "ACME_KEY_PATH", "file": "acme.pem", "prompt": "A"},
             {"env": "ACME_OTHER_PATH", "file": "ACME.PEM", "prompt": "B"}`,
			"file",
		},
		// The same bug one layer along: both land in the deployment's .env,
		// where the second wins and the first route reads the wrong path.
		"duplicate env across secrets": {
			`{"env": "ACME_KEY_PATH", "file": "a.pem", "prompt": "A"},
             {"env": "ACME_KEY_PATH", "file": "b.pem", "prompt": "B"}`,
			"env",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := strings.Replace(validManifest,
				`{"env": "ACME_KEY_PATH", "file": "acme.pem", "prompt": "ACME private key", "multiline": true}`,
				c.insert, 1)
			if in == validManifest {
				t.Fatal("fixture no longer contains the secret being replaced")
			}
			_, err := decode(t, in)
			if err == nil {
				t.Fatal("want a refusal for a manifest that declares one name twice")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error should name the field, got: %v", err)
			}
		})
	}
}

// A secret and a config sharing an env name is the same collision: config is
// written into the same .env, so it would replace the path to a credential
// with a literal.
func TestValidateRejectsAConfigThatShadowsASecret(t *testing.T) {
	in := strings.Replace(validManifest, `"env": "ACME_APP_ID"`, `"env": "ACME_KEY_PATH"`, 1)
	if in == validManifest {
		t.Fatal("fixture no longer contains the config env")
	}
	if _, err := decode(t, in); err == nil {
		t.Fatal("want a refusal when a config reuses a secret's env name")
	}
}
