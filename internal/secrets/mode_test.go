package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The gap a live run exposed: Apply chmods what it writes, but a credential
// that is REUSED is never written, so its mode was never checked. A file that
// arrived at 0644 by some other route stayed that way while every install
// reported success.
func TestEnsureSecretModeOnlyTightens(t *testing.T) {
	dir := t.TempDir()

	cases := map[string]struct {
		start     os.FileMode
		want      os.FileMode
		tightened bool
	}{
		"world readable": {0o644, 0o600, true},
		"group readable": {0o640, 0o600, true},
		"world writable": {0o666, 0o600, true},
		"already tight":  {0o600, 0o600, false},
		"stricter still": {0o400, 0o400, false}, // stricter than required is fine
	}

	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(label, " ", "-"))
			if err := os.WriteFile(path, []byte("secret"), c.start); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, c.start); err != nil {
				t.Fatal(err)
			}

			tightened, err := EnsureMode(path)
			if err != nil {
				t.Fatal(err)
			}
			if tightened != c.tightened {
				t.Errorf("tightened = %v, want %v", tightened, c.tightened)
			}

			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != c.want {
				t.Errorf("mode = %o, want %o", got, c.want)
			}
		})
	}
}
