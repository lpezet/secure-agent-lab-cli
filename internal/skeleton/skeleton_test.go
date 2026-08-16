package skeleton

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A skeleton shaped like the stack's, under a placeholder that is deliberately
// NOT the one upstream ships. Every assertion below would still pass against a
// build that hardcoded "acme" if the fixture used "acme" too — so it does not.
func fixture(t *testing.T, placeholder string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, up := placeholder, strings.ToUpper(placeholder)

	write("provider.json", `{
  "schema_version": 1,
  "name": "`+p+`",
  "load_band": "provider",
  "hosts": ["api.`+p+`.invalid"],
  "secrets": [{"env": "`+up+`_TOKEN_PATH", "file": "`+p+`.token", "prompt": "TODO"}],
  "broker_routes": [{"path": "/`+p+`/cred", "exposed": false}]
}`)
	write("broker/"+p+".js", `const audit = require("../audit");
module.exports = {
  "/`+p+`/cred": async (url, send) => {
    const token = tryReadFile(process.env.`+up+`_TOKEN_PATH);
    audit({ provider: "`+p+`", event: "cred_issued" });
  },
};`)
	write("proxy/"+p+".py", `HOSTS = ("api.`+p+`.invalid",)
def request(flow):
    audit(provider="`+p+`", event="cred_injected")
    # your provider decides what to attach here
`)
	return dir
}

func render(t *testing.T, placeholder, name string) map[string]string {
	t.Helper()
	files, err := Render(fixture(t, placeholder), name)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.Path] = f.Body
	}
	return out
}

// THE requirement this whole change turns on. The placeholder belongs to the
// stack: it was proposed as `__PROVIDER__` and shipped as `acme`, and a sal
// with either baked in renders a broken entry the day the other is used —
// silently, because the result is still valid JSON and valid Python.
func TestThePlaceholderIsReadFromTheSkeletonsOwnManifest(t *testing.T) {
	// The third is the token the proposal used, and it is not idle: `_` is a
	// word character to Go, so a Title-case substitution branch left it
	// unchanged and renamed the entry to `Telegraph`. Kept as a case because
	// it caught that.
	for _, placeholder := range []string{"acme", "boilerplate", "__provider__"} {
		files := render(t, placeholder, "telegraph")
		for path, body := range files {
			if strings.Contains(body, placeholder) {
				t.Errorf("placeholder %q survived in %s", placeholder, path)
			}
			if strings.Contains(path, "Telegraph") || strings.Contains(body, `"Telegraph"`) {
				t.Errorf("placeholder %q: the entry was renamed to a name it does not have, in %s", placeholder, path)
			}
		}
		if !strings.Contains(files["provider.json"], `"name": "telegraph"`) {
			t.Errorf("placeholder %q: manifest does not name the new entry", placeholder)
		}
	}
}

// Left behind, the broker reads an environment variable the manifest never
// declares: it installs fine, finds no credential, and says nothing.
func TestTheUpperCaseFormIsSubstitutedToo(t *testing.T) {
	files := render(t, "acme", "telegraph")

	for path, body := range files {
		if strings.Contains(body, "ACME") {
			t.Errorf("%s still names the skeleton's env var", path)
		}
	}
	if !strings.Contains(files["provider.json"], "TELEGRAPH_TOKEN_PATH") {
		t.Error("the manifest does not declare the renamed env var")
	}
	if !strings.Contains(files["broker/telegraph.js"], "TELEGRAPH_TOKEN_PATH") {
		t.Error("the broker does not read the renamed env var")
	}
}

// A hyphen is legal in an entry name and illegal in an environment variable,
// so the upper form maps it rather than producing something no shell can set.
func TestAHyphenatedNameStillYieldsALegalEnvVar(t *testing.T) {
	files := render(t, "acme", "my-thing")

	if !strings.Contains(files["provider.json"], "MY_THING_TOKEN_PATH") {
		t.Errorf("env var not mapped:\n%s", files["provider.json"])
	}
	if strings.Contains(files["provider.json"], "MY-THING") {
		t.Error("a hyphen reached an environment variable name")
	}
	// The lower form keeps the hyphen: it names files, routes and hosts, all
	// of which allow one.
	if _, ok := files["broker/my-thing.js"]; !ok {
		t.Errorf("files = %v, want broker/my-thing.js", keys(files))
	}
}

// The filename carries the token too, and an installer looks the file up by
// the manifest's name — so a body renamed without its filename produces an
// entry that installs nothing at all.
func TestFilenamesAreRenamedWithTheContents(t *testing.T) {
	files := render(t, "acme", "telegraph")

	for _, want := range []string{"provider.json", "broker/telegraph.js", "proxy/telegraph.py"} {
		if _, ok := files[want]; !ok {
			t.Errorf("missing %s; got %v", want, keys(files))
		}
	}
	// provider.json is a fixed filename and must NOT be renamed, which falls
	// out of substituting only the placeholder — and is worth asserting,
	// because a blanket rename of the word "provider" did exactly this once.
	for path := range files {
		if strings.Contains(path, "telegraph.json") {
			t.Errorf("the manifest was renamed to %s", path)
		}
	}
}

// The word "provider" appears throughout and none of its occurrences may move.
// `load_band` is a schema enum value, `provider=` is the audit trail's field
// NAME rather than its contents, and the rest is English. An earlier skeleton
// used "provider" as the placeholder and a tool doing the obvious substitution
// corrupted all four.
func TestTheWordProviderNeverMoves(t *testing.T) {
	files := render(t, "acme", "telegraph")

	if !strings.Contains(files["provider.json"], `"load_band": "provider"`) {
		t.Error("load_band is no longer the enum value the schema requires")
	}
	if !strings.Contains(files["proxy/telegraph.py"], `provider="telegraph"`) {
		t.Error("the addon should log provider=<name>, keyword and all")
	}
	if !strings.Contains(files["broker/telegraph.js"], `provider: "telegraph"`) {
		t.Error("the broker should log { provider: <name> }, keyword and all")
	}
	if !strings.Contains(files["proxy/telegraph.py"], "your provider") {
		t.Error("prose that says \"your provider\" should not be renamed")
	}
}

func TestNameIsValidatedBeforeItReachesAPath(t *testing.T) {
	dir := fixture(t, "acme")
	for _, bad := range []string{"../escape", "Not A Name", "", "9lives", "-leading"} {
		if _, err := Render(dir, bad); err == nil {
			t.Errorf("Render(%q) was accepted; it is about to become a directory name", bad)
		}
	}
}

// A skeleton whose manifest sal cannot read is refused rather than rendered
// with a guessed token, which would leave the placeholder in place.
func TestASkeletonWithNoReadablePlaceholderIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "provider.json"), []byte(`{"schema_version": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(dir, "telegraph"); err == nil {
		t.Error("a skeleton declaring no name was accepted")
	}

	empty := t.TempDir()
	if _, err := Render(empty, "telegraph"); err == nil {
		t.Error("a skeleton with no manifest at all was accepted")
	}
}

func TestWriteRefusesAnExistingDirectory(t *testing.T) {
	src := fixture(t, "acme")
	dst := t.TempDir() // already exists

	_, err := Write(src, dst, "telegraph")
	var exists *ErrExists
	if err == nil || !asErrExists(err, &exists) {
		t.Fatalf("err = %v, want ErrExists — writing over somebody's work is not recoverable", err)
	}
}

func asErrExists(err error, target **ErrExists) bool {
	e, ok := err.(*ErrExists)
	if ok {
		*target = e
	}
	return ok
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
