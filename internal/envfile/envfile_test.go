package envfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvFileUpsertPreservesTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	original := "# a comment worth keeping\nEXISTING=untouched\nTARGET=old\n\n# trailing note\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Upsert(path, map[string]string{"TARGET": "new", "ADDED": "value"}); err != nil {
		t.Fatal(err)
	}

	got := mustRead(t, path)
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
	err := Upsert(path, map[string]string{"K": "one\ntwo"})
	if err == nil {
		t.Fatal("want an error: a newline turns the rest of the value into another assignment")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("the file was written despite the refusal")
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An absent file is an empty set rather than an error: a virgin deployment has
// nothing in it, and every caller would otherwise have to special-case that.
func TestReadIsEmptyForAMissingFile(t *testing.T) {
	values, err := Read(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("got %v, want nothing", values)
	}
}

// An empty value is a value. `sal features disable` writes COMPOSE_PROFILES=
// to mean "every feature off", and a reader that could not tell that from an
// absent variable would read it as "every feature on" — turning the audit
// trail back on behind the operator.
func TestAnEmptyValueIsNotAnAbsentOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("COMPOSE_PROFILES=\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	values, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	value, present := values["COMPOSE_PROFILES"]
	if !present {
		t.Fatal("an assignment with an empty value must still be present")
	}
	if value != "" {
		t.Errorf("value = %q, want empty", value)
	}
}
