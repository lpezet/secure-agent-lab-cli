// Package envfile reads and writes the KEY=VALUE files a deployment keeps.
//
// Two of them, and keeping them apart is a boundary property rather than
// tidiness: .env is the broker's and proxy's environment, lab.env is the lab
// container's, and the lab must never receive the broker's.
package envfile

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

// Upsert sets keys in a KEY=VALUE file, leaving everything else alone.
//
// Rewriting the file wholesale would be simpler and wrong: the file is shared
// by every provider installed into the deployment, and an operator may have
// edited a value by hand. Existing keys are updated in place so ordering and
// comments survive, and new ones are appended.
func Upsert(path string, updates map[string]string) error {
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
		if k, ok := key(line); ok {
			if val, found := remaining[k]; found {
				fmt.Fprintf(&out, "%s=%s\n", k, val)
				delete(remaining, k)
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

// key returns the key of an assignment line, if it is one. Comments and
// blank lines are not.
func key(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	k, _, found := strings.Cut(trimmed, "=")
	if !found {
		return "", false
	}
	k = strings.TrimSpace(k)
	if k == "" {
		return "", false
	}
	return k, true
}

// Read returns the assignments in a KEY=VALUE file. A missing file is
// an empty set rather than an error: a virgin deployment has nothing in it.
func Read(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}

	values := map[string]string{}
	scan := bufio.NewScanner(bytes.NewReader(b))
	for scan.Scan() {
		line := scan.Text()
		k, ok := key(line)
		if !ok {
			continue
		}
		_, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		values[k] = value
	}
	return values, scan.Err()
}
