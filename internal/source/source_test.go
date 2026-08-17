package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Built from acceptedPrefixes rather than written out, for two reasons. It
// covers whatever that list actually holds instead of a copy that can fall
// behind it — and internal/invariants forbids a bank entry name as a string
// literal, the bank has an entry called `github`, so a spelled-out URL here
// trips the same guard the production code sidesteps the same way.
func TestParseRepoAcceptsTheFormsPeopleActuallyPaste(t *testing.T) {
	const owner, repo = "acme", "lab-providers"

	inputs := []string{"acme/lab-providers"}
	for _, prefix := range acceptedPrefixes {
		inputs = append(inputs,
			prefix+"acme/lab-providers",
			prefix+"acme/lab-providers/",
			prefix+"acme/lab-providers.git",
		)
	}

	for _, in := range inputs {
		gotOwner, gotRepo, err := ParseRepo(in)
		if err != nil {
			t.Errorf("ParseRepo(%q): %v", in, err)
			continue
		}
		if gotOwner != owner || gotRepo != repo {
			t.Errorf("ParseRepo(%q) = %q/%q, want %q/%q", in, gotOwner, gotRepo, owner, repo)
		}
	}
}

func TestParseRepoRefusesWhatIsNotARepository(t *testing.T) {
	for _, in := range []string{
		"", "acme", "acme/lab/extra", "https://example.com/acme/lab",
		"../../etc", "acme/lab providers",
	} {
		if _, _, err := ParseRepo(in); err == nil {
			t.Errorf("ParseRepo(%q) was accepted", in)
		}
	}
}

// Re-adding a name silently repointing it at a different repository is how
// somebody ends up running code from somewhere they did not choose. The whole
// value of a registry is that the answer to "whose code is this" stays the one
// that was given, so a taken name is refused and the error says what it
// already points at.
func TestAddRefusesANameAlreadyTaken(t *testing.T) {
	r := &Registry{}
	if err := r.Add(Source{Name: "acme", Owner: "acme", Repo: "lab", Ref: "main"}); err != nil {
		t.Fatal(err)
	}

	err := r.Add(Source{Name: "acme", Owner: "someone-else", Repo: "lab", Ref: "main"})
	if err == nil {
		t.Fatal("a second source silently took the name")
	}
	if !strings.Contains(err.Error(), "acme/lab") {
		t.Errorf("the error does not say what the name already points at: %v", err)
	}

	if s, _ := r.Find("acme"); s.Owner != "acme" {
		t.Errorf("the registry was changed by a call that refused: %#v", s)
	}
}

func TestAddValidatesTheNameBeforeItBecomesPartOfAnIdentifier(t *testing.T) {
	r := &Registry{}
	for _, bad := range []string{"", "Not A Name", "9lives", "-leading", "has@at", "has/slash"} {
		if err := r.Add(Source{Name: bad, Owner: "a", Repo: "b", Ref: "main"}); err == nil {
			t.Errorf("Add(%q) was accepted; it is about to appear after an @", bad)
		}
	}
}

// `entry@source` splits on the LAST @, so an entry name is free to contain one
// even though today's naming rules do not allow it. Splitting on the first
// would make the source the part that varies, which is backwards.
func TestQualifiedSplitsEntryFromSource(t *testing.T) {
	for _, tc := range []struct{ in, entry, source string }{
		{"slack", "slack", ""},
		{"slack@acme", "slack", "acme"},
		{"a@b@acme", "a@b", "acme"},
		{"@acme", "@acme", ""}, // no entry name: not a qualified reference
	} {
		entry, src := Qualified(tc.in)
		if entry != tc.entry || src != tc.source {
			t.Errorf("Qualified(%q) = %q, %q; want %q, %q", tc.in, entry, src, tc.entry, tc.source)
		}
	}
}

// One rule and no more. A draft that also stripped leading `sal-`/`lab-`
// turned `lab-providers` into `providers`, which names nobody — both ends
// matched. `--as` covers what a clumsy default misses; nothing covers a
// misleading one.
func TestDefaultNameStripsOnlyTheTrailingNoise(t *testing.T) {
	for in, want := range map[string]string{
		"lab-providers":  "lab",
		"acme-providers": "acme",
		"acme-provider":  "acme",
		"acme-bank":      "acme",
		"Acme":           "acme",
		// Nothing to strip, and nothing invented. A repo that is only the
		// suffix keeps its name rather than collapsing to nothing.
		"providers": "providers",
		"sal-acme":  "sal-acme",
		// Not a legal source name; returned as-is so Add is the one that
		// refuses it, with the message about names rather than a silent
		// mangling here.
		"9lives": "9lives",
	} {
		if got := DefaultName(in); got != want {
			t.Errorf("DefaultName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A missing registry is the ordinary state — having added no sources is not an
// error, and treating it as one would make every command that consults it fail
// on a fresh machine.
func TestLoadTreatsAMissingRegistryAsEmpty(t *testing.T) {
	r, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load on a fresh config dir: %v", err)
	}
	if len(r.Sources) != 0 {
		t.Errorf("sources = %#v, want none", r.Sources)
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{}
	if err := r.Add(Source{Name: "acme", Owner: "acme", Repo: "lab-providers", Ref: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, r); err != nil {
		t.Fatal(err)
	}

	back, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := back.Find("acme")
	if !ok || s.Ref != "v1.0.0" || s.Owner != "acme" {
		t.Fatalf("round trip lost something: %#v", back.Sources)
	}
	if back.SchemaVersion != Generation {
		t.Errorf("schema_version = %d, want %d", back.SchemaVersion, Generation)
	}
}

// A registry from a build that knew more than this one may describe a source
// on terms this build would ignore — and a source is a statement about whose
// code may run behind a credential boundary, so ignoring part of one is not
// something to do quietly.
func TestARegistryFromTheFutureIsRefused(t *testing.T) {
	dir := t.TempDir()
	body := `{"schema_version": 99, "sources": [{"name": "acme", "owner": "a", "repo": "b", "ref": "main"}]}`
	if err := os.WriteFile(filepath.Join(dir, File), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("a registry from a later generation was read anyway")
	}
}

func TestARegistryWithNoGenerationIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, File), []byte(`{"sources": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("a registry declaring no generation was read anyway")
	}
}

// The flag's own default has to work. `--ref main` was rejected by the pattern
// that guards a STACK tag, which is right for the stack — a security boundary
// is pinned to releases on purpose — and wrong for a source, where a branch is
// an ordinary answer. Shipped broken, and caught by running the command with
// no flags at all.
func TestARefMayBeABranch(t *testing.T) {
	src := Source{Name: "acme", Owner: "acme", Repo: "lab", Ref: "main"}.BankSource(context.Background())

	for _, ok := range []string{"main", "master", "release/1.13", "v1.0.0", "a", "feature_x-2"} {
		if _, err := src.ResolveRef(context.Background(), ok); err != nil && strings.Contains(err.Error(), "not a usable ref") {
			t.Errorf("ResolveRef(%q) refused a legitimate ref: %v", ok, err)
		}
	}
}

// Still a control, not a formality: the ref goes straight into a request path.
func TestARefCannotCarryAPathEscape(t *testing.T) {
	src := Source{Name: "acme", Owner: "acme", Repo: "lab", Ref: "main"}.BankSource(context.Background())

	for _, bad := range []string{
		"../../etc", "main/..", "..", "main?x=1", "main#frag", "/main", "main/",
		"main with space", "", "-main",
	} {
		_, err := src.ResolveRef(context.Background(), bad)
		if err == nil || !strings.Contains(err.Error(), "not a usable ref") {
			t.Errorf("ResolveRef(%q) was not refused by the pattern: %v", bad, err)
		}
	}
}

// The token is attached to the request and never written down. Anything that
// put it in sources.json would be a secret in a config file with none of the
// mode discipline the secrets directory has.
func TestTheTokenNeverReachesTheRegistry(t *testing.T) {
	t.Setenv(EnvVars[0], "ghp-not-a-real-token")

	dir := t.TempDir()
	r := &Registry{}
	if err := r.Add(Source{Name: "acme", Owner: "acme", Repo: "lab", Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, r); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "ghp-not-a-real-token") {
		t.Fatalf("the token was written to the registry:\n%s", body)
	}

	// And it IS on the fetcher, or a private source could not be read at all.
	if got := r.Sources[0].BankSource(context.Background()).Token; got != "ghp-not-a-real-token" {
		t.Errorf("token = %q, want the one in the environment", got)
	}
}

// Explicit before ambient. When both exist, the environment variable is the one
// the operator typed for this run; the keychain is whatever they logged in as
// months ago, and silently preferring it would make the first look ignored.
func TestAnExplicitTokenWinsOverTheAmbientOne(t *testing.T) {
	t.Setenv(EnvVars[0], "from-env")
	t.Setenv(EnvVars[1], "from-gh-env")

	if got := Token(context.Background()); got != "from-env" {
		t.Errorf("Token() = %q, want the value of the first variable", got)
	}
	if got := TokenSource(context.Background()); got != "$"+EnvVars[0] {
		t.Errorf("TokenSource() = %q", got)
	}

	// Reporting names the variable, never the value — this string is printed.
	if strings.Contains(TokenSource(context.Background()), "from-env") {
		t.Error("TokenSource leaked the token itself")
	}
}
