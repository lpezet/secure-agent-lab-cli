package compose

import (
	"encoding/json"
	"testing"
)

// Running decides whether a lab counts as "currently running with my
// credentials attached", so both directions of it are load-bearing: a false
// negative reports a live credential-injecting proxy as idle, and a false
// positive makes the warning noise.
func TestProjectRunning(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
	}{
		{"running(6)", true},
		// A lab that came up halfway is as much a live credential path as one
		// that came up completely.
		{"running(3), exited(3)", true},
		{"exited(6)", false},
		{"created(6)", false},
		{"paused(6)", false},
		// No other state docker reports contains the word, which is what makes
		// the substring safe — restarting in particular does not.
		{"restarting(2)", false},
		{"", false},
	} {
		if got := (Project{Status: tc.status}).Running(); got != tc.want {
			t.Errorf("Project{Status: %q}.Running() = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// docker capitalises these keys, and Go's decoder is case-insensitive on field
// names but not on the shape of the document. This pins the three fields sal
// reads: silently decoding to zero values would report every lab as unknown.
func TestProjectDecodesDockersKeys(t *testing.T) {
	const out = `[{"Name":"api-abcd1234","Status":"running(6)","ConfigFiles":"/labs/api-abcd1234/compose.yaml"}]`

	var got []Project
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d projects, want 1", len(got))
	}
	if got[0].Name != "api-abcd1234" || !got[0].Running() || got[0].ConfigFiles == "" {
		t.Errorf("decoded %+v", got[0])
	}
}
