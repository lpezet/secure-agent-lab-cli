package installer

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
)

// upsertEnvFile sets keys in a KEY=VALUE file, leaving everything else alone.
//
// Rewriting the file wholesale would be simpler and wrong: the file is shared
// by every provider installed into the deployment, and an operator may have
// edited a value by hand. Existing keys are updated in place so ordering and
// comments survive, and new ones are appended.
func upsertEnvFile(path string, updates map[string]string) error {
	for k, v := range updates {
		if strings.ContainsAny(v, "\n\r") {
			// A newline would silently truncate the value and turn the rest
			// into another assignment.
			return fmt.Errorf("value for %s contains a newline, which an env file cannot represent", k)
		}
	}

	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	remaining := make(map[string]string, len(updates))
	for k, v := range updates {
		remaining[k] = v
	}

	var out bytes.Buffer
	scan := bufio.NewScanner(bytes.NewReader(existing))
	for scan.Scan() {
		line := scan.Text()
		if key, ok := envKey(line); ok {
			if val, found := remaining[key]; found {
				fmt.Fprintf(&out, "%s=%s\n", key, val)
				delete(remaining, key)
				continue
			}
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if err := scan.Err(); err != nil {
		return err
	}

	if len(remaining) > 0 {
		keys := make([]string, 0, len(remaining))
		for k := range remaining {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&out, "%s=%s\n", k, remaining[k])
		}
	}

	// 0600: this holds paths into the secrets directory and whatever config an
	// operator typed, and it sits in a directory that is 0700 anyway.
	return os.WriteFile(path, out.Bytes(), 0o600)
}

// envKey returns the key of an assignment line, if it is one. Comments and
// blank lines are not.
func envKey(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	key, _, found := strings.Cut(trimmed, "=")
	if !found {
		return "", false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false
	}
	return key, true
}
