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
	p, err := BuildPlan(openBank(t, "local-stack/bank"), "acme", emptyRecord(), emptyRecord().UsedSlots(), "v1.9.0")
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
		"lab/setup.d/acme.sh":    0o755, // named per entry, or two providers' setup.sh collide
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
	p, err := BuildPlan(openBank(t, "local-stack/bank"), "widget", emptyRecord(), emptyRecord().UsedSlots(), "v1.9.0")
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
	_, err := BuildPlan(openBank(t, "refused"), "leaky-conf", emptyRecord(), nil, "v1.9.0")
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
	_, err := BuildPlan(openBank(t, "refused"), "host-mismatch", emptyRecord(), nil, "v1.9.0")
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
			_, err := BuildPlan(openBank(t, "refused"), dir, emptyRecord(), nil, "v1.9.0")
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

	_, err := BuildPlan(openBank(t, "local-stack/bank"), "acme", rec, rec.UsedSlots(), "v1.9.0")
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

	p, err := BuildPlan(openBank(t, "local-stack/bank"), "acme", rec, rec.UsedSlots(), "v1.9.0")
	if err != nil {
		t.Fatal(err)
	}
	if p.Slot != 12 {
		t.Errorf("slot = %d, want the gap at 12", p.Slot)
	}
}

// A stray addon the record does not know about must not have its number
// reused, or the new file silently shadows it in load order.
//
// And the record must come back UNCHANGED. An earlier version folded what it
// found on disk into rec.Installed, and the caller saved that — writing the
// stack's own addons into installed.json as bank entries that do not exist.
func TestOccupiedSlotsReadsDiskWithoutTouchingTheRecord(t *testing.T) {
	dep := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dep, "proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"000_policy.py", "001_allowlist.py", "010_stray.py", "notanaddon.py", "README.md"} {
		if err := os.WriteFile(filepath.Join(dep, "proxy", name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rec := emptyRecord()
	occupied := OccupiedSlots(dep, rec)

	if len(rec.Installed) != 0 {
		t.Fatalf("the record gained %d entries; OccupiedSlots must not write to it", len(rec.Installed))
	}
	for _, want := range []int{0, 1, 10} {
		if _, ok := occupied[want]; !ok {
			t.Errorf("slot %03d on disk was not reported as occupied", want)
		}
	}

	p, err := BuildPlan(openBank(t, "local-stack/bank"), "acme", rec, occupied, "v1.9.0")
	if err != nil {
		t.Fatal(err)
	}
	if p.Slot != 11 {
		t.Errorf("slot = %d, want 11: 010 is taken on disk", p.Slot)
	}
}

func TestApplyWritesEverything(t *testing.T) {
	dep, secrets := t.TempDir(), t.TempDir()

	p, err := BuildPlan(openBank(t, "local-stack/bank"), "acme", emptyRecord(), emptyRecord().UsedSlots(), "v1.9.0")
	if err != nil {
		t.Fatal(err)
	}

	entry, err := p.Apply(dep, secrets, "v1.9.0", Values{
		Secrets: map[string][]byte{"ACME_PRIVATE_KEY_PATH": []byte("--TEST KEY BLOB--\n")},
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

	p, err := BuildPlan(openBank(t, "local-stack/bank"), "acme", emptyRecord(), emptyRecord().UsedSlots(), "v1.9.0")
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// upgradeFixture builds a deployment holding one entry at an older release,
// with a file the newer release no longer ships.
func upgradeFixture(t *testing.T) (deployDir string, rec *deployment.Record) {
	t.Helper()
	deployDir = t.TempDir()
	for _, d := range []string{"broker", "proxy", "cred-gateway"} {
		if err := os.MkdirAll(filepath.Join(deployDir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		"broker/acme.js", "proxy/010_acme.py", "cred-gateway/acme.conf",
		"proxy/000_policy.py", "proxy/002_retired.py",
	} {
		if err := os.WriteFile(filepath.Join(deployDir, f), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec = &deployment.Record{
		StackTag:   "v1.8.0",
		BaseAddons: []string{"000_policy.py", "002_retired.py"},
		Installed: []deployment.Entry{{
			Name: "acme", Slot: 10, SchemaVersion: 1,
			Files: []string{"broker/acme.js", "proxy/010_acme.py", "cred-gateway/acme.conf"},
		}},
	}
	return deployDir, rec
}

func addonsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"000_policy.py", "001_allowlist.py"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("addon"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The slot must survive an upgrade. It is the addon's filename prefix and so
// its load order, and load order is a security property — the policy band runs
// before providers deliberately.
func TestUpgradeKeepsTheAssignedSlot(t *testing.T) {
	_, rec := upgradeFixture(t)
	rec.Installed[0].Slot = 42

	u, err := BuildUpgradePlan(openBank(t, "local-stack/bank"), addonsFixture(t), rec, "v1.9.0", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Entries) != 1 {
		t.Fatalf("planned %d entries, want 1", len(u.Entries))
	}
	if got := u.Entries[0].Plan.Slot; got != 42 {
		t.Errorf("slot = %d, want the recorded 42", got)
	}
	var addon string
	for _, f := range u.Entries[0].Plan.Files {
		if strings.HasPrefix(f.Dst, "proxy/") {
			addon = f.Dst
		}
	}
	if addon != "proxy/042_acme.py" {
		t.Errorf("addon = %q, want proxy/042_acme.py", addon)
	}
}

// One provider that cannot make the move refuses the whole upgrade. Half a
// deployment on each of two releases is a boundary nobody can describe.
func TestUpgradeRefusesWhollyWhenAnEntryCannotMove(t *testing.T) {
	_, rec := upgradeFixture(t)
	rec.Installed = append(rec.Installed, deployment.Entry{Name: "widget", Slot: 11, SchemaVersion: 1})

	// widget declares min_stack 1.9.0, so a move to 1.8.0 is impossible.
	_, err := BuildUpgradePlan(openBank(t, "local-stack/bank"), addonsFixture(t), rec, "v1.8.0", "")
	if err == nil {
		t.Fatal("want a refusal naming the provider that cannot move")
	}
	for _, want := range []string{"widget", "Nothing has been changed", "sal providers remove"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// The deletion half of an upgrade, which is the half that carries the risk: a
// gateway config the new release stopped shipping keeps whitelisting a route
// the entry no longer exposes.
func TestUpgradeDeletesFilesTheNewReleaseDropped(t *testing.T) {
	deployDir, rec := upgradeFixture(t)

	// widget ships no cred-gateway config, so standing in for a release where
	// acme dropped one, its file set is the smaller one.
	rec.Installed[0].Name = "widget"
	rec.Installed[0].Files = []string{"broker/widget.js", "proxy/010_widget.py", "cred-gateway/widget.conf"}
	for _, f := range rec.Installed[0].Files {
		if err := os.WriteFile(filepath.Join(deployDir, f), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	u, err := BuildUpgradePlan(openBank(t, "local-stack/bank"), addonsFixture(t), rec, "v1.9.0", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Entries[0].Stale; len(got) != 1 || got[0] != "cred-gateway/widget.conf" {
		t.Fatalf("stale = %v, want [cred-gateway/widget.conf]", got)
	}
	if got := u.StaleAddons; len(got) != 1 || got[0] != "002_retired.py" {
		t.Fatalf("stale addons = %v, want [002_retired.py]", got)
	}

	newRec, err := u.Apply(deployDir, t.TempDir(), map[string]Values{})
	if err != nil {
		t.Fatal(err)
	}

	for _, gone := range []string{"cred-gateway/widget.conf", "proxy/002_retired.py"} {
		if _, err := os.Stat(filepath.Join(deployDir, gone)); err == nil {
			t.Errorf("%s survived the upgrade", gone)
		}
	}
	// And the new release's addons are present.
	if _, err := os.Stat(filepath.Join(deployDir, "proxy", "001_allowlist.py")); err != nil {
		t.Errorf("new addon missing: %v", err)
	}
	if newRec.StackTag != "v1.9.0" {
		t.Errorf("record tag = %q", newRec.StackTag)
	}
}

// Config an operator already set must not be re-prompted; only what a new
// release added.
func TestUpgradeAsksOnlyForNewConfig(t *testing.T) {
	deployDir, rec := upgradeFixture(t)
	if err := os.WriteFile(filepath.Join(deployDir, ".env"), []byte("ACME_APP_ID=already-set\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	u, err := BuildUpgradePlan(openBank(t, "local-stack/bank"), addonsFixture(t), rec, "v1.9.0", "")
	if err != nil {
		t.Fatal(err)
	}
	needed, err := u.NewConfig(deployDir)
	if err != nil {
		t.Fatal(err)
	}

	// acme declares ACME_APP_ID and ACME_REGION; only the unset one is wanted.
	got := needed["acme"]
	if len(got) != 1 || got[0] != "ACME_REGION" {
		t.Errorf("needs %v, want only [ACME_REGION]", got)
	}
}
