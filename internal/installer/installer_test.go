package installer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lpezet/secure-agent-lab-cli/internal/bank"
	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
)

func openBank(t *testing.T, which string) *bank.Bank {
	t.Helper()
	b, err := bank.OpenDir(filepath.Join("..", "..", "tests", "fixtures", which))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func emptyRecord() *deployment.Record {
	return &deployment.Record{StackTag: "v1.9.0", Installed: []deployment.Entry{}}
}

func TestPlanForFullEntry(t *testing.T) {
	p, err := BuildPlan(openBank(t, "local-stack/bank"), "acme", emptyRecord(), "v1.9.0")
	if err != nil {
		t.Fatal(err)
	}

	// Lowest free number in the provider band.
	if p.Slot != 10 {
		t.Errorf("slot = %d, want 10", p.Slot)
	}

	got := map[string]os.FileMode{}
	for _, f := range p.Files {
		got[f.Dst] = f.Mode
	}
	want := map[string]os.FileMode{
		"broker/acme.js":         0o644,
		"proxy/010_acme.py":      0o644, // the installer assigns NNN; the bank never bakes one in
		"cred-gateway/acme.conf": 0o644,
		"lab/acme.sh":            0o755, // named per entry, or two providers' setup.sh collide
	}
	if len(got) != len(want) {
		t.Fatalf("planned %v, want %v", got, want)
	}
	for dst, mode := range want {
		if got[dst] != mode {
			t.Errorf("%s mode = %o, want %o", dst, got[dst], mode)
		}
	}

	// A secret's env var names a PATH into the mount, never the value.
	if p.SecretEnv["ACME_PRIVATE_KEY_PATH"] != "/secrets/acme-app.pem" {
		t.Errorf("secret env = %q", p.SecretEnv["ACME_PRIVATE_KEY_PATH"])
	}
	if len(p.LabEnv) != 2 {
		t.Errorf("lab env = %v", p.LabEnv)
	}
}

// An entry with no exposed routes ships no gateway config, and that is a shape
// the planner must accept rather than treat as incomplete.
func TestPlanForMinimalEntry(t *testing.T) {
	p, err := BuildPlan(openBank(t, "local-stack/bank"), "widget", emptyRecord(), "v1.9.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range p.Files {
		if strings.HasPrefix(f.Dst, "cred-gateway/") {
			t.Errorf("planned a gateway config for an entry that exposes nothing: %s", f.Dst)
		}
	}
	if len(p.SecretEnv) != 0 || len(p.LabEnv) != 0 {
		t.Error("minimal entry should declare no secrets and no lab env")
	}
}

// The control this whole package exists to enforce.
func TestPlanRefusesWhitelistedUnexposedRoute(t *testing.T) {
	_, err := BuildPlan(openBank(t, "refused"), "leaky-conf", emptyRecord(), "v1.9.0")
	if err == nil {
		t.Fatal("want refusal: the manifest marks a route exposed:false and the conf whitelists it")
	}
	for _, want := range []string{"/leaky/token", "exposed:false", "reusable credential"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// The other half of entry consistency, and the quieter failure of the two: a
// host the manifest declares but the addon never matches means the credential
// is simply never injected. Nothing errors — the request goes out bare, or the
// vendor rejects it much later and somewhere else.
func TestPlanRefusesHostTheAddonNeverMatches(t *testing.T) {
	_, err := BuildPlan(openBank(t, "refused"), "host-mismatch", emptyRecord(), "v1.9.0")
	if err == nil {
		t.Fatal("want refusal: the manifest declares a host the addon does not mention")
	}
	if !strings.Contains(err.Error(), "uploads.ghost.example") {
		t.Errorf("error should name the unmatched host, got: %v", err)
	}
	// The host the addon DOES match must not be reported as missing.
	if strings.Contains(err.Error(), "api.ghost.example,") || strings.Contains(err.Error(), " api.ghost.example ") {
		t.Errorf("error names a host that is matched, got: %v", err)
	}
}

func TestPlanRefusesBadManifests(t *testing.T) {
	cases := map[string]string{
		"future-schema": "99",
		"stack-too-new": "99.0.0",
	}
	for dir, want := range cases {
		t.Run(dir, func(t *testing.T) {
			_, err := BuildPlan(openBank(t, "refused"), dir, emptyRecord(), "v1.9.0")
			if err == nil {
				t.Fatal("want refusal")
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q, got: %v", want, err)
			}
		})
	}
}

func TestPlanRefusesReinstall(t *testing.T) {
	rec := emptyRecord()
	rec.Installed = append(rec.Installed, deployment.Entry{Name: "acme", Slot: 10})

	_, err := BuildPlan(openBank(t, "local-stack/bank"), "acme", rec, "v1.9.0")
	if !errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("err = %v, want ErrAlreadyInstalled", err)
	}
}

func TestSlotAssignmentTakesLowestFree(t *testing.T) {
	rec := emptyRecord()
	rec.Installed = append(rec.Installed,
		deployment.Entry{Name: "one", Slot: 10},
		deployment.Entry{Name: "two", Slot: 11},
		deployment.Entry{Name: "four", Slot: 13},
	)

	p, err := BuildPlan(openBank(t, "local-stack/bank"), "acme", rec, "v1.9.0")
	if err != nil {
		t.Fatal(err)
	}
	if p.Slot != 12 {
		t.Errorf("slot = %d, want the gap at 12", p.Slot)
	}
}

// A stray addon the record does not know about must not have its number
// reused, or the new file silently shadows it in load order.
func TestObserveOnDiskSlots(t *testing.T) {
	dep := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dep, "proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"010_stray.py", "011_other.py", "notanaddon.py", "README.md"} {
		if err := os.WriteFile(filepath.Join(dep, "proxy", name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rec := emptyRecord()
	ObserveOnDiskSlots(dep, rec)

	p, err := BuildPlan(openBank(t, "local-stack/bank"), "acme", rec, "v1.9.0")
	if err != nil {
		t.Fatal(err)
	}
	if p.Slot != 12 {
		t.Errorf("slot = %d, want 12: 010 and 011 exist on disk", p.Slot)
	}
}

func TestApplyWritesEverything(t *testing.T) {
	dep, secrets := t.TempDir(), t.TempDir()

	p, err := BuildPlan(openBank(t, "local-stack/bank"), "acme", emptyRecord(), "v1.9.0")
	if err != nil {
		t.Fatal(err)
	}

	entry, err := p.Apply(dep, secrets, "v1.9.0", Values{
		Secrets: map[string][]byte{"ACME_PRIVATE_KEY_PATH": []byte("-----BEGIN KEY-----\n")},
		Config:  map[string]string{"ACME_APP_ID": "12345", "ACME_REGION": "us-east-1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The credential is 0600 and lives outside the deployment.
	info, err := os.Stat(filepath.Join(secrets, "acme-app.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secret mode = %o, want 600", perm)
	}

	// The addon landed under its assigned number.
	if _, err := os.Stat(filepath.Join(dep, "proxy", "010_acme.py")); err != nil {
		t.Error(err)
	}

	env := readFile(t, filepath.Join(dep, ".env"))
	for _, want := range []string{
		"ACME_PRIVATE_KEY_PATH=/secrets/acme-app.pem", // a path, never the value
		"ACME_APP_ID=12345",
		"ACME_REGION=us-east-1",
	} {
		if !strings.Contains(env, want) {
			t.Errorf(".env missing %q; got:\n%s", want, env)
		}
	}
	// The credential itself must never reach a file the broker reads as config.
	if strings.Contains(env, "BEGIN KEY") {
		t.Error("the secret's VALUE was written into .env")
	}

	labEnv := readFile(t, filepath.Join(dep, "lab.env"))
	if !strings.Contains(labEnv, "ACME_CREDENTIAL_URL=") {
		t.Errorf("lab.env missing the manifest's lab_env; got:\n%s", labEnv)
	}
	// The lab must not receive the broker's environment.
	if strings.Contains(labEnv, "ACME_PRIVATE_KEY_PATH") {
		t.Error("a secret path leaked into lab.env")
	}

	if entry.Slot != 10 || entry.SchemaVersion != 1 || len(entry.Files) != 4 {
		t.Errorf("record entry = %+v", entry)
	}
}

// Modes must be corrected on a file that already exists, not just on creation.
func TestApplyTightensAnExistingSecret(t *testing.T) {
	dep, secrets := t.TempDir(), t.TempDir()
	loose := filepath.Join(secrets, "acme-app.pem")
	if err := os.WriteFile(loose, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := BuildPlan(openBank(t, "local-stack/bank"), "acme", emptyRecord(), "v1.9.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(dep, secrets, "v1.9.0", Values{
		Secrets: map[string][]byte{"ACME_PRIVATE_KEY_PATH": []byte("new")},
	}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(loose)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600: a credential that was world-readable must not stay so", perm)
	}
}

func TestEnvFileUpsertPreservesTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	original := "# a comment worth keeping\nEXISTING=untouched\nTARGET=old\n\n# trailing note\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := upsertEnvFile(path, map[string]string{"TARGET": "new", "ADDED": "value"}); err != nil {
		t.Fatal(err)
	}

	got := readFile(t, path)
	for _, want := range []string{
		"# a comment worth keeping",
		"EXISTING=untouched",
		"TARGET=new",
		"# trailing note",
		"ADDED=value",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "TARGET=old") {
		t.Error("the old value survived alongside the new one")
	}
	// Updated in place rather than appended, so an operator's ordering holds.
	if strings.Index(got, "TARGET=new") > strings.Index(got, "# trailing note") {
		t.Error("TARGET moved to the end instead of being updated where it was")
	}
}

func TestEnvFileRefusesNewlines(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	err := upsertEnvFile(path, map[string]string{"K": "one\ntwo"})
	if err == nil {
		t.Fatal("want an error: a newline turns the rest of the value into another assignment")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("the file was written despite the refusal")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The gap a live run exposed: Apply chmods what it writes, but a credential
// that is REUSED is never written, so its mode was never checked. A file that
// arrived at 0644 by some other route stayed that way while every install
// reported success.
func TestEnsureSecretModeOnlyTightens(t *testing.T) {
	dir := t.TempDir()

	cases := map[string]struct {
		start     os.FileMode
		want      os.FileMode
		tightened bool
	}{
		"world readable": {0o644, 0o600, true},
		"group readable": {0o640, 0o600, true},
		"world writable": {0o666, 0o600, true},
		"already tight":  {0o600, 0o600, false},
		"stricter still": {0o400, 0o400, false}, // stricter than required is fine
	}

	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(label, " ", "-"))
			if err := os.WriteFile(path, []byte("secret"), c.start); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, c.start); err != nil {
				t.Fatal(err)
			}

			tightened, err := EnsureSecretMode(path)
			if err != nil {
				t.Fatal(err)
			}
			if tightened != c.tightened {
				t.Errorf("tightened = %v, want %v", tightened, c.tightened)
			}

			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != c.want {
				t.Errorf("mode = %o, want %o", got, c.want)
			}
		})
	}
}
