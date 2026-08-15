package drift

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deployment builds a temp deployment holding the given files, and a temp
// "release" to compare it against.
func deployment(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for path, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func reference(t *testing.T, files map[string]string) string {
	t.Helper()
	return deployment(t, files)
}

func kindOf(t *testing.T, r *Report, path string) Kind {
	t.Helper()
	for _, f := range r.Findings {
		if f.Path == path {
			return f.Kind
		}
	}
	t.Fatalf("no finding for %q; got %v", path, r.Findings)
	return ""
}

func TestCheckClassifiesEveryState(t *testing.T) {
	ref := reference(t, map[string]string{
		"acme.js":      "the release's copy\n",
		"policy.py":    "the release's policy\n",
		"gateway.conf": "the release's config\n",
	})
	deploy := deployment(t, map[string]string{
		"broker/acme.js":           "the release's copy\n",
		"proxy/000_policy.py":      "SOMEONE EDITED THIS\n",
		"cred-gateway/legacy.conf": "a config the release stopped shipping\n",
		"cred-gateway/rogue.conf":  "location = /acme/token { proxy_pass http://broker:8080; }\n",
	})

	report, err := Check(deploy, []Expected{
		{Path: "broker/acme.js", Src: filepath.Join(ref, "acme.js"), Owner: "bank/acme/broker/"},
		{Path: "proxy/000_policy.py", Src: filepath.Join(ref, "policy.py"), Owner: "stack/proxy/addons/"},
		{Path: "cred-gateway/acme.conf", Src: filepath.Join(ref, "gateway.conf"), Owner: "bank/acme/cred-gateway/"},
		{Path: "cred-gateway/legacy.conf", Owner: "bank/acme"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]Kind{
		"broker/acme.js":           OK,
		"proxy/000_policy.py":      Drift,
		"cred-gateway/acme.conf":   Missing,
		"cred-gateway/legacy.conf": Stale,
		"cred-gateway/rogue.conf":  Unowned,
	} {
		if got := kindOf(t, report, path); got != want {
			t.Errorf("%s = %s, want %s", path, got, want)
		}
	}

	if !report.Failed() {
		t.Error("a deployment in this state is not what it claims to be")
	}
}

// A file nothing installed is a finding, not a note. In a hand-rolled
// deployment a file with no upstream counterpart is ordinary; in a sal-managed
// one every boundary file was written down, so one that was not arrived some
// other way — and the thing most likely to have added a .conf to
// cred-gateway/ is the agent the boundary exists to contain.
func TestAnUnrecordedFileIsAFinding(t *testing.T) {
	deploy := deployment(t, map[string]string{
		"cred-gateway/extra.conf": "location = /anything { proxy_pass http://broker:8080; }\n",
	})

	report, err := Check(deploy, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := kindOf(t, report, "cred-gateway/extra.conf"); got != Unowned {
		t.Errorf("got %s, want %s", got, Unowned)
	}
	if !report.Failed() {
		t.Error("an unowned file in a managed directory must not exit clean")
	}
}

// lab/ is the operator's build context: its Dockerfile is theirs by design, so
// judging what is in there would report their own file as an intrusion. Files
// sal installed into it are still compared — that is a different question.
func TestTheLabDirectoryIsNotJudged(t *testing.T) {
	deploy := deployment(t, map[string]string{
		"lab/Dockerfile": "FROM debian:stable-slim\n",
	})

	report, err := Check(deploy, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range report.Findings {
		if strings.HasPrefix(f.Path, "lab/") {
			t.Errorf("lab/ should not be scanned, but reported: %+v", f)
		}
	}
}

// An entry the release cannot produce leaves its files unjudgeable. Reporting
// them as unowned would say the operator smuggled in what sal itself
// installed, so the caller says it once about the record instead.
func TestFilesOfAnUnresolvableEntryAreNotUnowned(t *testing.T) {
	deploy := deployment(t, map[string]string{
		"proxy/010_vanished.py": "# from an entry that no longer exists\n",
	})

	report, err := Check(deploy, nil, []string{"proxy/010_vanished.py"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 {
		t.Errorf("expected no findings for a file whose owner is known, got %+v", report.Findings)
	}
}

// A stale file that is already gone is not a finding: there is nothing left to
// keep whitelisting anything.
func TestAStaleFileThatIsAlreadyGoneIsSilent(t *testing.T) {
	report, err := Check(t.TempDir(), []Expected{{Path: "cred-gateway/legacy.conf"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 {
		t.Errorf("expected silence, got %+v", report.Findings)
	}
	if report.Failed() {
		t.Error("nothing was wrong here")
	}
}

func TestDiffShowsTheChangedLines(t *testing.T) {
	got := DiffBytes(
		[]byte("one\ntwo\nthree\n"),
		[]byte("one\ntwo CHANGED\nthree\n"),
	)
	if !strings.Contains(got, "-two\n") || !strings.Contains(got, "+two CHANGED\n") {
		t.Errorf("diff did not show the change:\n%s", got)
	}
	// The common prefix and suffix are trimmed, so an unchanged line does not
	// appear as a change.
	if strings.Contains(got, "-one") || strings.Contains(got, "+three") {
		t.Errorf("diff reported unchanged lines:\n%s", got)
	}
}
