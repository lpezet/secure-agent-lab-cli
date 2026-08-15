package scaffold

import (
	"encoding/json"
	"strings"
	"testing"
)

func render(t *testing.T, name string) map[string]string {
	t.Helper()
	files, err := Files(name)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.Path] = f.Body
	}
	return out
}

// stripComments removes Python comment lines and docstrings, the way the
// stack's own inv_pretty_host does before it looks for the anti-pattern. Every
// shipped addon explains that anti-pattern in its own prose, which is the
// point — the rule is about code, not about whether the word appears.
func stripComments(body string) string {
	var kept []string
	inDoc := false
	for _, line := range strings.Split(body, "\n") {
		if strings.Count(line, `"""`)%2 == 1 {
			inDoc = !inDoc
			continue
		}
		if inDoc {
			continue
		}
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// THE rule this scaffold got wrong. flow.request.pretty_host prefers the
// client-supplied Host header, so a lab container can point a request at its
// own server, spoof the header, and have the real credential injected into a
// request that never reaches the vendor. The stack lints every addon it ships
// for this; a scaffold that teaches it hands the mistake to whoever copies it.
func TestTheProxyAddonDecidesOnTheRealHost(t *testing.T) {
	code := stripComments(render(t, "telegraph")["proxy/telegraph.py"])

	if strings.Contains(code, "pretty_host") {
		t.Error("the addon decides on pretty_host, which the client can spoof")
	}
	if !strings.Contains(code, "flow.request.host") {
		t.Error("the addon should decide on flow.request.host")
	}
	// Case, a trailing root dot and a :port all have to be normalised before
	// comparing. A plain == against a lowercase name is what let
	// http://BROKER:8080/… through the internal-host block until stack 1.9.2.
	if !strings.Contains(code, "hostmatch.matches") {
		t.Error("host comparison should go through hostmatch, not a bare ==")
	}
}

// The other thing it got wrong, and the reason it went unnoticed: the broker
// loads a provider with Object.assign(routes, require(file)), so the export
// must be the route keys themselves. Wrapping them in a `routes` object
// registers one route literally called "routes" and never calls the handlers —
// an install that succeeds and a provider that answers nothing.
func TestTheBrokerProviderExportsRouteKeys(t *testing.T) {
	code := render(t, "telegraph")["broker/telegraph.js"]

	if !strings.Contains(code, `"/telegraph/cred": async (url, send)`) {
		t.Error("the export should map a route path to an (url, send) handler")
	}
	for _, wrong := range []string{"routes:", "(req, res)", "res.end("} {
		if strings.Contains(code, wrong) {
			t.Errorf("%q is not the broker's provider API", wrong)
		}
	}
}

// The stack asserts this over every entry it ships, in both directions: a host
// declared and not matched means the credential is silently never injected,
// and a host matched but not declared is an injection the egress allowlist was
// never seeded for. A scaffold that failed it would fail `sal providers add`.
func TestDeclaredHostsAreTheHostsMatched(t *testing.T) {
	files := render(t, "telegraph")

	var manifest struct {
		Hosts        []string `json:"hosts"`
		LoadBand     string   `json:"load_band"`
		BrokerRoutes []struct {
			Path    string `json:"path"`
			Exposed bool   `json:"exposed"`
		} `json:"broker_routes"`
	}
	if err := json.Unmarshal([]byte(files["provider.json"]), &manifest); err != nil {
		t.Fatalf("the scaffolded manifest does not parse: %v", err)
	}

	code := stripComments(files["proxy/telegraph.py"])
	for _, host := range manifest.Hosts {
		if !strings.Contains(code, host) {
			t.Errorf("manifest declares %q, which the addon never matches", host)
		}
	}

	// load_band is a schema enum value that happens to read like the
	// placeholder. Substituting it would produce a manifest the installer
	// refuses — and this scaffold did exactly that kind of blanket rename
	// once.
	if manifest.LoadBand != "provider" {
		t.Errorf("load_band = %q, want the enum value \"provider\"", manifest.LoadBand)
	}
}

// A static key is a reusable secret, and the stack's rule is that a route
// handing one over stays unexposed. So this shape exposes nothing and ships no
// cred-gateway config — a scaffold with an exposed route would teach the one
// mistake `sal providers add` refuses an entry for.
func TestTheStaticKeyShapeExposesNothing(t *testing.T) {
	files := render(t, "telegraph")

	var manifest struct {
		BrokerRoutes []struct {
			Path    string `json:"path"`
			Exposed bool   `json:"exposed"`
		} `json:"broker_routes"`
	}
	if err := json.Unmarshal([]byte(files["provider.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	for _, r := range manifest.BrokerRoutes {
		if r.Exposed {
			t.Errorf("%s is exposed; a static key is reusable and must not reach the lab", r.Path)
		}
	}
	for path := range files {
		if strings.HasPrefix(path, "cred-gateway/") {
			t.Errorf("%s whitelists a route this shape does not expose", path)
		}
	}
}

// The audit field is named `provider` in every writer in the stack, and the
// trail's shape is what observer and anything reading it depend on. A blanket
// rename of the word turned that keyword into the vendor's name, which is the
// bug these tests were written after.
func TestTheAuditFieldKeepsItsName(t *testing.T) {
	files := render(t, "telegraph")

	if !strings.Contains(files["proxy/telegraph.py"], `provider="telegraph"`) {
		t.Error("the addon should log provider=<name>, keyword and all")
	}
	if !strings.Contains(files["broker/telegraph.js"], `{ provider: "telegraph"`) {
		t.Error("the broker should log { provider: <name> }, keyword and all")
	}
	// And the English prose survives: it is about the reader's provider, not
	// about a thing called telegraph.
	if !strings.Contains(files["proxy/telegraph.py"], "your provider") {
		t.Error("prose that says \"your provider\" should not be renamed")
	}
}

func TestNameIsValidatedBeforeItReachesAPath(t *testing.T) {
	for _, bad := range []string{"../escape", "Not A Name", "", "9lives", "-leading"} {
		if _, err := Files(bad); err == nil {
			t.Errorf("Files(%q) was accepted; it is about to become a directory name", bad)
		}
	}
}
