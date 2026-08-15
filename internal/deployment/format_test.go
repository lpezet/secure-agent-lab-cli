package deployment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The exact shape of generation 1, as COMPATIBILITY.md publishes it.
//
// This file is a cross-repo contract: scripts/check-drift.sh in the stack repo
// reads it, run by someone who may never have installed sal. So a change to the
// field set is a change to another repository's input, and this test exists to
// make that change impossible to make by accident — it fails on a new field, a
// renamed one, or a dropped one, and whoever wrote it then has to decide which
// kind of change they are making.
//
// Adding an OPTIONAL field is not a generation event (this file is decoded
// plainly, so an older sal ignores what it does not know). Adding a REQUIRED
// one, or changing what an existing field means, is.
func TestTheRecordFormatIsWhatIsPublished(t *testing.T) {
	dir := t.TempDir()
	rec := &Record{
		StackTag:    "v1.9.0",
		StackCommit: "0123456789abcdef0123456789abcdef01234567",
		ProjectDir:  "/home/dev/projects/api",
		Installed: []Entry{{
			Name:          "acme",
			Slot:          10,
			SchemaVersion: 1,
			Files:         []string{"broker/acme.js", "proxy/010_acme.py"},
			StackTag:      "v1.9.0",
			Source:        SourceLocal,
			InstalledAt:   time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
		}},
		BaseAddons: []string{"000_policy.py"},
	}
	if err := Save(dir, rec); err != nil {
		t.Fatal(err)
	}

	var raw map[string]any
	read(t, Path(dir), &raw)

	want := []string{
		"schema_version", "stack_tag", "stack_commit",
		"project_dir", "installed", "base_addons",
	}
	if diff := fieldDiff(raw, want); diff != "" {
		t.Errorf("record fields changed:\n%s\n\nIf this is deliberate, update COMPATIBILITY.md — and if the "+
			"new field is REQUIRED, or changes what an existing one means, it is a generation event.", diff)
	}

	entries, ok := raw["installed"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("installed = %v, want one entry", raw["installed"])
	}
	entry, _ := entries[0].(map[string]any)
	wantEntry := []string{
		"name", "slot", "schema_version", "files",
		"stack_tag", "source", "installed_at",
	}
	if diff := fieldDiff(entry, wantEntry); diff != "" {
		t.Errorf("entry fields changed:\n%s", diff)
	}
}

// The optional fields are omitted rather than written empty, which is what
// lets a reader tell "this sal did not record it" from "it is empty".
func TestOptionalFieldsAreOmittedWhenUnset(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, &Record{StackTag: "v1.9.0", Installed: []Entry{}}); err != nil {
		t.Fatal(err)
	}

	var raw map[string]any
	read(t, Path(dir), &raw)

	for _, absent := range []string{"stack_commit", "project_dir", "base_addons"} {
		if _, present := raw[absent]; present {
			t.Errorf("%s was written even though it was not set", absent)
		}
	}
	// installed is required and stays, empty: "no providers" and "this file
	// does not say" are different answers, and check-drift.sh reads this field.
	if _, present := raw["installed"]; !present {
		t.Error("installed must always be written, even empty")
	}
}

// THE forward-compatibility claim, stated in COMPATIBILITY.md: an older sal
// meeting a field it does not know ignores it and still answers correctly. If
// this ever stops being true, adding a field to this file becomes a generation
// event and the rule in the docs is wrong.
func TestAnUnknownFieldIsIgnoredRatherThanRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{
	  "schema_version": 1,
	  "stack_tag": "v1.9.0",
	  "installed": [{"name": "acme", "slot": 10, "schema_version": 1, "files": [], "future_field": true}],
	  "something_a_later_sal_added": {"nested": ["anything"]}
	}`
	if err := os.WriteFile(Path(dir), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	rec, err := Load(dir)
	if err != nil {
		t.Fatalf("a record with unknown fields must still load: %v", err)
	}
	if len(rec.Installed) != 1 || rec.Installed[0].Name != "acme" {
		t.Errorf("the parts this build understands must still be read: %+v", rec.Installed)
	}
}

// A generation above what this build supports is refused rather than read —
// it may carry a field this sal would silently ignore. The rule itself lives
// in internal/schema; this checks that Load actually applies it, since a
// record that skipped the check would be the one place it mattered.
func TestARecordFromTheFutureIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"schema_version": 99, "stack_tag": "v1.9.0", "installed": []}`
	if err := os.WriteFile(Path(dir), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(dir); err == nil {
		t.Fatal("a record from a later generation must be refused, not read")
	}
}

func read(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatal(err)
	}
}

// fieldDiff reports what is present and unexpected, and what is expected and
// missing.
func fieldDiff(got map[string]any, want []string) string {
	expected := make(map[string]bool, len(want))
	for _, w := range want {
		expected[w] = true
	}

	var added, missing []string
	for k := range got {
		if !expected[k] {
			added = append(added, k)
		}
	}
	for _, w := range want {
		if _, present := got[w]; !present {
			missing = append(missing, w)
		}
	}
	sort.Strings(added)
	sort.Strings(missing)

	var b strings.Builder
	if len(added) > 0 {
		b.WriteString("  unexpected: " + strings.Join(added, ", ") + "\n")
	}
	if len(missing) > 0 {
		b.WriteString("  missing:    " + strings.Join(missing, ", ") + "\n")
	}
	return b.String()
}
