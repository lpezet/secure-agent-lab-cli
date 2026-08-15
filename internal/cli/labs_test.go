package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
)

// The recorded project directory is a CLAIM made when the lab was created, and
// every way it can stop being true describes the same thing: a deployment
// nothing is using, which may still be running with the secrets directory
// mounted. Each has to be distinguishable, because each has a different fix.
func TestDescribeProjectChecksTheClaim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	project := filepath.Join(home, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	l := &lab.Lab{Name: "project-abcd1234", Dir: filepath.Join(home, "labs", "project-abcd1234")}

	t.Run("a project that points back", func(t *testing.T) {
		if err := lab.WritePointer(project, lab.Pointer{Name: l.Name, StackTag: "v1.9.0"}); err != nil {
			t.Fatal(err)
		}
		got := describeProject(l, &deployment.Record{ProjectDir: project})
		if got != project {
			t.Errorf("a healthy lab must print its project and nothing else, got %q", got)
		}
	})

	t.Run("a project that now uses another lab", func(t *testing.T) {
		if err := lab.WritePointer(project, lab.Pointer{Name: "somewhere-else", StackTag: "v1.9.0"}); err != nil {
			t.Fatal(err)
		}
		if got := describeProject(l, &deployment.Record{ProjectDir: project}); !strings.Contains(got, "somewhere-else") {
			t.Errorf("got %q, want the lab that project moved to", got)
		}
	})

	t.Run("a project with no pointer left", func(t *testing.T) {
		if err := os.Remove(filepath.Join(project, lab.PointerDir, lab.PointerFile)); err != nil {
			t.Fatal(err)
		}
		if got := describeProject(l, &deployment.Record{ProjectDir: project}); !strings.Contains(got, "nothing is using this lab") {
			t.Errorf("got %q, want the finding", got)
		}
	})

	t.Run("a project that is gone", func(t *testing.T) {
		got := describeProject(l, &deployment.Record{ProjectDir: filepath.Join(home, "deleted")})
		if !strings.Contains(got, "gone") {
			t.Errorf("got %q, want the finding", got)
		}
	})

	// Absent rather than wrong: a lab created before sal recorded the field.
	// It must not read as a finding, and it must say what fills it in.
	t.Run("a lab that predates the field", func(t *testing.T) {
		got := describeProject(l, &deployment.Record{})
		if strings.Contains(got, "nothing is using this lab") {
			t.Errorf("an unrecorded project is not a forgotten lab: %q", got)
		}
		if !strings.Contains(got, "sal upgrade") {
			t.Errorf("got %q, want the command that records it", got)
		}
	})
}

// The pointer check must not walk up. A project inside another project's tree
// would otherwise be answered by the ancestor's pointer, and a lab nothing
// points at would report healthy — the one answer this command exists to
// refuse to give.
func TestDescribeProjectDoesNotAcceptAnAncestorsPointer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	outer := filepath.Join(home, "outer")
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := lab.WritePointer(outer, lab.Pointer{Name: "inner-abcd1234", StackTag: "v1.9.0"}); err != nil {
		t.Fatal(err)
	}

	l := &lab.Lab{Name: "inner-abcd1234", Dir: filepath.Join(home, "labs", "inner-abcd1234")}
	got := describeProject(l, &deployment.Record{ProjectDir: inner})
	if !strings.Contains(got, "no .sal/lab.json") {
		t.Errorf("got %q — the ancestor's pointer was accepted for the inner project", got)
	}
}

// Only projects running out of the labs directory are sal's business. Warning
// about every compose project on the machine would make the one finding that
// matters unreadable.
func TestUnderDir(t *testing.T) {
	root := filepath.FromSlash("/home/u/.config/secure-agent-lab/labs")
	for _, tc := range []struct {
		name        string
		configFiles string
		want        bool
	}{
		{"a lab", filepath.Join(root, "api-abcd1234", "compose.yaml"), true},
		{"someone else's app", filepath.FromSlash("/srv/app/compose.yaml"), false},
		// Compose reports several files comma-separated, and any one of them
		// under the labs directory makes the project ours.
		{"an override file", filepath.FromSlash("/srv/app/compose.yaml,") + filepath.Join(root, "api-abcd1234", "override.yaml"), true},
		{"a sibling of the labs directory", filepath.FromSlash("/home/u/.config/secure-agent-lab/secrets/compose.yaml"), false},
		{"nothing at all", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := underDir(tc.configFiles, root); got != tc.want {
				t.Errorf("underDir(%q) = %v, want %v", tc.configFiles, got, tc.want)
			}
		})
	}
}

func TestDescribeProviders(t *testing.T) {
	// "none" and "not reported" must not look the same: one says the lab has
	// no credential path installed, the other would say nothing at all.
	if got := describeProviders(&deployment.Record{}); !strings.Contains(got, "none") {
		t.Errorf("got %q, want an explicit none", got)
	}
	rec := &deployment.Record{Installed: []deployment.Entry{{Name: "widget"}, {Name: "acme"}}}
	if got := describeProviders(rec); got != "acme, widget" {
		t.Errorf("got %q, want them sorted", got)
	}
}
