package compose

import (
	"bufio"
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden compose file")

func sampleData() Data {
	return Data{
		ProjectName:   "acme-3f9a2c1b",
		ProjectDir:    "/home/dev/projects/acme",
		SecretsDir:    "/home/dev/.config/secure-agent-lab/secrets",
		StackTag:      "v1.9.0",
		LabDockerfile: false,
	}
}

func render(t *testing.T, d Data) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, d); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return buf.String()
}

// The golden file is the reviewable artefact. A change to the compose template
// changes the security boundary of every lab sal creates, so it should be read
// as a diff rather than trusted because the tests still pass.
func TestRenderGolden(t *testing.T) {
	got := render(t, sampleData())
	path := filepath.Join("testdata", "golden", "compose.yaml")

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("golden updated; read the diff before committing it")
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run: go test ./internal/compose -update)", err)
	}
	if got != string(want) {
		t.Errorf("rendered compose differs from %s; run with -update and review the diff", path)
	}
}

// The guard the AST invariant test cannot provide.
//
// internal/invariants only parses .go files, so a provider name in a template
// would sail past it — and a templated provider name is exactly the coupling
// this repo is built to prevent. Checking the OUTPUT rather than the template
// also catches a name arriving through Data.
func TestNoProviderNamesInOutput(t *testing.T) {
	names := bankNames(t)
	if len(names) == 0 {
		t.Fatal("no bank names loaded; this test would be vacuous")
	}

	out := render(t, sampleData())

	// The stack repo's own URL is the one legitimate occurrence of a name that
	// is also a bank entry — it names the host the stack is published on.
	// Remove it before looking, rather than exempting the whole line.
	out = strings.ReplaceAll(out, StackRepo, "")

	for _, tok := range regexp.MustCompile(`[^a-z0-9]+`).Split(strings.ToLower(out), -1) {
		if names[tok] {
			t.Errorf("rendered compose names bank entry %q; per-provider values belong in .env and lab.env, written from the manifest", tok)
		}
	}
}

func bankNames(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "invariants", "testdata", "bank-names.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	names := map[string]bool{}
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			names[strings.ToLower(line)] = true
		}
	}
	return names
}

// Structural properties that must survive any edit to the template. The golden
// file would catch a change to these too, but only as "something differs" —
// these say which invariant broke and why it matters.
func TestRenderedInvariants(t *testing.T) {
	out := render(t, sampleData())

	t.Run("observer port is not chosen by sal", func(t *testing.T) {
		if !strings.Contains(out, `"127.0.0.1::9000"`) {
			t.Error("observer should publish 127.0.0.1::9000 and let Docker assign the host port")
		}
		if regexp.MustCompile(`"127\.0\.0\.1:\d+:9000"`).MatchString(out) {
			t.Error("a fixed host port makes collisions between labs possible")
		}
	})

	t.Run("observer and log-rotator are on no network", func(t *testing.T) {
		for _, svc := range []string{"observer", "log-rotator"} {
			if networksOf(out, svc) != "" {
				t.Errorf("%s must be on neither network: it reaches the audit volume without becoming a channel between the two sides", svc)
			}
		}
	})

	t.Run("lab is internal and sees only the lab network", func(t *testing.T) {
		if !strings.Contains(out, "internal: ${LAB_INTERNAL:-true}") {
			t.Error("the lab network must default to internal, or the proxy is advisory")
		}
		if got := networksOf(out, "lab"); got != "[lab]" {
			t.Errorf("lab service networks = %q, want [lab]", got)
		}
	})

	t.Run("the broker mounts secrets, not its parent", func(t *testing.T) {
		if !strings.Contains(out, "/home/dev/.config/secure-agent-lab/secrets:/secrets:ro") {
			t.Error("secrets mount missing or not read-only")
		}
		if strings.Contains(out, "/home/dev/.config/secure-agent-lab:/secrets") {
			t.Error("mounting the parent would put the bank cache and every other lab in the broker")
		}
	})

	t.Run("every service is pinned to the same release", func(t *testing.T) {
		refs := regexp.MustCompile(`\.git#(\S+?):`).FindAllStringSubmatch(out, -1)
		if len(refs) != 6 {
			t.Fatalf("found %d pinned build refs, want 6 services", len(refs))
		}
		for _, m := range refs {
			if m[1] != "v1.9.0" {
				t.Errorf("service pinned to %q, want v1.9.0", m[1])
			}
		}
	})

	t.Run("no per-provider environment", func(t *testing.T) {
		// A path into /secrets in this file means a provider's secret was
		// templated instead of written to .env from its manifest.
		if strings.Contains(out, "/secrets/") {
			t.Error("a secret path appears in the compose file; those come from manifests via .env")
		}
	})
}

func TestRenderWithLocalLabDockerfile(t *testing.T) {
	d := sampleData()
	d.LabDockerfile = true
	out := render(t, d)

	if !strings.Contains(out, "build: ./lab") {
		t.Error("with a deployment-owned Dockerfile the lab should build locally")
	}
	// The other five services stay pinned to the stack.
	if n := strings.Count(out, ".git#v1.9.0:stack/"); n != 5 {
		t.Errorf("%d services still pinned, want 5", n)
	}
}

func TestRenderRefusesIncompleteData(t *testing.T) {
	cases := map[string]func(*Data){
		"no project name": func(d *Data) { d.ProjectName = "" },
		"no stack tag":    func(d *Data) { d.StackTag = "" },
		"relative project dir": func(d *Data) {
			// Would resolve against the deployment directory and mount the
			// wrong tree at /workspace, silently.
			d.ProjectDir = "../acme"
		},
		"relative secrets dir": func(d *Data) { d.SecretsDir = "secrets" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := sampleData()
			mutate(&d)
			if err := Render(new(bytes.Buffer), d); err == nil {
				t.Error("want an error rather than a compose file with a hole in it")
			}
		})
	}
}

// networksOf returns the `networks:` value for a service block, or "" if the
// service declares none.
func networksOf(out, service string) string {
	lines := strings.Split(out, "\n")
	inService := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "  "+service+":"):
			inService = true
			continue
		case inService && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(strings.TrimSpace(line), ":"):
			return "" // next service began
		case inService && strings.HasPrefix(strings.TrimSpace(line), "networks:"):
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "networks:"))
		}
	}
	return ""
}

// A feature is a compose profile, and `sal features enable|disable NAME` acts
// on the SERVICE of the same name — that equivalence is the whole
// implementation, and it lives in this template rather than in code. A profile
// naming nothing would give a feature that lists, enables and disables while
// turning nothing on or off.
//
// It also checks the other direction: every profile must be in
// DefaultProfiles, because a profile this build does not know about ships
// disabled on a new lab. For anything shaped like the observer that is a lab
// quietly serving no audit trail.
func TestEveryProfileIsAService(t *testing.T) {
	out := render(t, sampleData())

	profiles := regexp.MustCompile(`(?m)^\s+profiles: \["([a-z0-9-]+)"\]`).FindAllStringSubmatch(out, -1)
	if len(profiles) == 0 {
		t.Fatal("no profiles in the rendered file; sal features would have nothing to operate on")
	}

	for _, m := range profiles {
		name := m[1]
		if !regexp.MustCompile(`(?m)^  ` + name + `:$`).MatchString(out) {
			t.Errorf("profile %q names no service; `sal features disable %s` would turn nothing off", name, name)
		}
		if !contains(DefaultProfiles, name) {
			t.Errorf("profile %q is not in DefaultProfiles, so a new lab ships with it off", name)
		}
	}

	for _, name := range DefaultProfiles {
		found := false
		for _, m := range profiles {
			found = found || m[1] == name
		}
		if !found {
			t.Errorf("DefaultProfiles names %q, which this template does not declare", name)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
