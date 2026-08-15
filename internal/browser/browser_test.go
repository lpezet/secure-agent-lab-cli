package browser

import (
	"reflect"
	"runtime"
	"testing"
)

func TestCandidatesPreferTheOperatorsChoice(t *testing.T) {
	t.Setenv("BROWSER", "my-browser")

	got := candidates("http://127.0.0.1:9000")
	if len(got) == 0 {
		t.Fatal("no candidates at all")
	}

	// First, and on every platform. $BROWSER is the only entry that can be
	// right on a machine where the usual launcher is missing or does nothing,
	// which is the case this whole design is shaped around.
	want := []string{"my-browser", "http://127.0.0.1:9000"}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("first candidate = %v, want %v", got[0], want)
	}
	if len(got) < 2 {
		t.Error("a bad $BROWSER should not be the only thing tried")
	}
}

func TestCandidatesFollowTheBrowserConvention(t *testing.T) {
	t.Setenv("BROWSER", "first %s --flag:second")

	got := candidates("URL")

	// %s substitution and the colon-separated list are the de facto $BROWSER
	// convention. Getting either wrong means launching with a URL in the
	// wrong place, or not launching at all.
	if want := []string{"first", "URL", "--flag"}; !reflect.DeepEqual(got[0], want) {
		t.Errorf("got[0] = %v, want %v", got[0], want)
	}
	if want := []string{"second", "URL"}; !reflect.DeepEqual(got[1], want) {
		t.Errorf("got[1] = %v, want %v", got[1], want)
	}
}

func TestCandidatesEndWithSomethingPlatformAppropriate(t *testing.T) {
	t.Setenv("BROWSER", "")

	got := candidates("URL")
	if len(got) == 0 {
		t.Fatal("no candidates at all")
	}

	first := got[0][0]
	switch runtime.GOOS {
	case "darwin":
		if first != "open" {
			t.Errorf("first candidate = %q, want open", first)
		}
	case "windows":
		if first != "rundll32" {
			t.Errorf("first candidate = %q, want rundll32", first)
		}
	default:
		// wslview ahead of xdg-open deliberately: on WSL xdg-open frequently
		// exists and opens nothing, which sal cannot detect.
		if first != "wslview" {
			t.Errorf("first candidate = %q, want wslview", first)
		}
	}
}
