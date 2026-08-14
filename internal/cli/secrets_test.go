package cli

import (
	"strings"
	"testing"

	"github.com/lpezet/secure-agent-lab-cli/internal/manifest"
)

// Two credentials on one provider, which is the shape that makes a selector
// necessary at all. Named for the fixture bank rather than any real entry —
// see internal/invariants.
func twoSecrets() *manifest.Manifest {
	return &manifest.Manifest{
		Name: "acme",
		Secrets: []manifest.Secret{
			{Env: "ACME_AUTH_TOKEN_PATH", File: "acme-auth.token", Prompt: "Acme OAuth token", Optional: true},
			{Env: "ACME_API_KEY_PATH", File: "acme.key", Prompt: "Acme API key", Optional: true},
		},
	}
}

// No selector is the hand-held path, and the reason `sal secrets set acme`
// needs no knowledge of what acme's credentials are: it walks what the
// manifest declares, in the manifest's order, with the manifest's wording.
func TestSelectSecretsWithoutASelectorWalksThemAll(t *testing.T) {
	m := twoSecrets()
	got, err := selectSecrets(m, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(m.Secrets) {
		t.Fatalf("got %d secrets, want %d", len(got), len(m.Secrets))
	}
	for i := range got {
		if got[i].Env != m.Secrets[i].Env {
			t.Errorf("position %d = %s, want %s — manifest order carries the provider's precedence rule", i, got[i].Env, m.Secrets[i].Env)
		}
	}
}

func TestSelectSecretsByName(t *testing.T) {
	cases := map[string]string{
		"exact":        "ACME_API_KEY_PATH",
		"any case":     "acme_api_key_path",
		"a unique bit": "api_key",
		"shorter":      "API",
	}

	for label, selector := range cases {
		t.Run(label, func(t *testing.T) {
			got, err := selectSecrets(twoSecrets(), selector)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Env != "ACME_API_KEY_PATH" {
				t.Fatalf("selector %q chose %+v", selector, got)
			}
		})
	}
}

// The property that matters most in this file. An ambiguous selector must be
// REFUSED, never resolved: picking wrong writes an OAuth token into the file
// the broker sends as an API key, and nothing reports that until a request is
// rejected — by which time the credential is stored, the lab is running, and
// the failure looks like a bad token rather than a misfiled one.
func TestSelectSecretsRefusesAmbiguity(t *testing.T) {
	_, err := selectSecrets(twoSecrets(), "ACME")
	if err == nil {
		t.Fatal("an ambiguous selector was resolved instead of refused")
	}
	// And the refusal has to be actionable, or it just moves the guessing to
	// the operator.
	for _, want := range []string{"ACME_AUTH_TOKEN_PATH", "ACME_API_KEY_PATH"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not list %s:\n%v", want, err)
		}
	}
}

// An exact match wins outright, so a full env name that happens to be a
// substring of another is never ambiguous.
func TestSelectSecretsPrefersAnExactMatch(t *testing.T) {
	m := &manifest.Manifest{
		Name: "acme",
		Secrets: []manifest.Secret{
			{Env: "ACME_KEY", File: "a.key", Prompt: "short"},
			{Env: "ACME_KEY_SECONDARY", File: "b.key", Prompt: "long"},
		},
	}

	got, err := selectSecrets(m, "ACME_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Env != "ACME_KEY" {
		t.Fatalf("chose %+v, want the exact match", got)
	}
}

func TestSelectSecretsRejectsAnUnknownName(t *testing.T) {
	_, err := selectSecrets(twoSecrets(), "NOPE")
	if err == nil {
		t.Fatal("an unknown selector was accepted")
	}
	if !strings.Contains(err.Error(), "ACME_AUTH_TOKEN_PATH") {
		t.Errorf("the error should show what IS available:\n%v", err)
	}
}

// A provider with one credential needs no selector, which is the whole of
// `sal secrets set <provider> --from-file <path>`.
func TestSelectSecretsWithASingleCredential(t *testing.T) {
	m := &manifest.Manifest{
		Name:    "widget",
		Secrets: []manifest.Secret{{Env: "WIDGET_KEY_PATH", File: "widget.pem", Prompt: "Widget key"}},
	}

	got, err := selectSecrets(m, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want the one declared credential", len(got))
	}
}

// The menu is what turns a refusal into something the operator can act on, so
// it carries the manifest's own wording rather than just the env names.
func TestSecretMenuCarriesTheManifestsPrompts(t *testing.T) {
	menu := secretMenu(twoSecrets())
	for _, want := range []string{"ACME_AUTH_TOKEN_PATH", "Acme OAuth token", "ACME_API_KEY_PATH", "Acme API key"} {
		if !strings.Contains(menu, want) {
			t.Errorf("menu is missing %q:\n%s", want, menu)
		}
	}
}

func TestSharedByOnlyWarnsWhenSharingIsReal(t *testing.T) {
	if got := sharedBy(nil); got != "" {
		t.Errorf("sharedBy(nil) = %q, want nothing to say", got)
	}
	if got := sharedBy([]string{"one"}); got != "" {
		t.Errorf("sharedBy(one lab) = %q; one lab is not sharing", got)
	}
	got := sharedBy([]string{"one", "two", "three"})
	if !strings.Contains(got, "3 labs") {
		t.Errorf("sharedBy(three labs) = %q, want the count", got)
	}
}
