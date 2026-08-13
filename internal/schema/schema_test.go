package schema

import (
	"strings"
	"testing"
)

func TestCheckAcceptsWhatThisBuildWrites(t *testing.T) {
	if err := Check("install record", Current, "/somewhere"); err != nil {
		t.Fatalf("this build must be able to read what it writes: %v", err)
	}
}

// Absence is refused rather than assumed to be the oldest generation. Guessing
// once would make the guess permanent — every later reader carrying the same
// branch forever.
func TestCheckRefusesAbsence(t *testing.T) {
	err := Check("lab pointer", 0, "/p/.sal/lab.json")
	if err == nil {
		t.Fatal("want a refusal for a file with no schema_version")
	}
	for _, want := range []string{"lab pointer", "/p/.sal/lab.json", "predates"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// The rule that matters: a file from the future is refused, not read for the
// parts this build happens to recognise.
func TestCheckRefusesTheFuture(t *testing.T) {
	err := Check("install record", 99, "/lab/.sal/installed.json")
	if err == nil {
		t.Fatal("want a refusal for a newer generation")
	}
	for _, want := range []string{"99", "does not understand", "upgrade sal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestSupportedIncludesCurrent(t *testing.T) {
	for _, v := range Supported {
		if v == Current {
			return
		}
	}
	t.Fatalf("Current=%d is not in Supported=%v; this build cannot read its own files", Current, Supported)
}
