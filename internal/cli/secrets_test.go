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

// A credential is named by its FILE. That name is self-explanatory in a way
// the env var is not, it is what `sal secrets list` prints, and it is what
// exists on disk.
func TestSelectSecretsByFile(t *testing.T) {
	for label, selector := range map[string]string{
		"as declared": "acme.key",
		"any case":    "ACME.KEY",
	} {
		t.Run(label, func(t *testing.T) {
			got, err := selectSecrets(twoSecrets(), selector)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].File != "acme.key" {
				t.Fatalf("selector %q chose %+v", selector, got)
			}
		})
	}
}

// Exact only. Every one of these named a real credential under the previous
// substring matcher, and each is now a refusal — because exactness is what
// makes the match unambiguous by construction rather than by tie-breaking.
func TestSelectSecretsIsExactOnly(t *testing.T) {
	for label, selector := range map[string]string{
		"a substring":        "auth",
		"without extension":  "acme-auth",
		"a different suffix": "acme-auth.tok",
		"a superstring":      "acme-auth.token.bak",
	} {
		t.Run(label, func(t *testing.T) {
			if got, err := selectSecrets(twoSecrets(), selector); err == nil {
				t.Fatalf("selector %q chose %+v, want a refusal", selector, got)
			}
		})
	}
}

// The env name must not work — it is the thing that started this — but "no
// credential named …" would say the credential does not exist, which is worse
// than the confusion it replaced. Someone typing it has read a deployment's
// .env and reasoned backwards, so the refusal points at the right name.
func TestSelectSecretsExplainsTheEnvName(t *testing.T) {
	_, err := selectSecrets(twoSecrets(), "ACME_AUTH_TOKEN_PATH")
	if err == nil {
		t.Fatal("the env name was accepted as a selector")
	}
	for _, want := range []string{
		"environment variable",  // what it actually is
		"path inside the conta", // and why its value is not what they have
		"acme-auth.token",       // the name that does work
		"Acme OAuth token",      // in the manifest's own words
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q:\n%v", want, err)
		}
	}
	// The message must point at the ONE credential they meant, not list both.
	if strings.Contains(err.Error(), "acme.key") {
		t.Errorf("the refusal names the other credential too, which is noise:\n%v", err)
	}
}

func TestSelectSecretsRejectsAnUnknownName(t *testing.T) {
	_, err := selectSecrets(twoSecrets(), "nosuchthing")
	if err == nil {
		t.Fatal("an unknown selector was accepted")
	}
	// An unrecognised name shows what IS available, in both columns.
	for _, want := range []string{"acme-auth.token", "acme.key", "Acme OAuth token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should show what is available; missing %q:\n%v", want, err)
		}
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

// The menu is what turns a refusal into something the operator can act on. It
// pairs the selector with the manifest's wording — the same two columns the
// interactive prompt shows, so what is read is what is typed.
func TestSecretMenuPairsFileWithPrompt(t *testing.T) {
	menu := secretMenu(twoSecrets())
	for _, want := range []string{"acme-auth.token", "Acme OAuth token", "acme.key", "Acme API key"} {
		if !strings.Contains(menu, want) {
			t.Errorf("menu is missing %q:\n%s", want, menu)
		}
	}
	// The env names are what the operator must NOT learn to type.
	for _, unwanted := range []string{"ACME_AUTH_TOKEN_PATH", "ACME_API_KEY_PATH"} {
		if strings.Contains(menu, unwanted) {
			t.Errorf("menu shows %q, which is the name this change exists to stop teaching:\n%s", unwanted, menu)
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
