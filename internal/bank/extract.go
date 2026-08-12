package bank

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits bound what an archive is allowed to be before extraction gives up.
//
// The bank is a handful of small text files, so these are generous by orders of
// magnitude and exist only to stop a pathological archive — a zip bomb, or one
// with a million entries — from filling a disk while sal looks like it is
// working.
type Limits struct {
	MaxTotalBytes int64
	MaxFileBytes  int64
	MaxEntries    int
}

// DefaultLimits are sized for a bank that is currently four entries of a few
// kilobytes each.
func DefaultLimits() Limits {
	return Limits{
		MaxTotalBytes: 64 << 20, // 64 MiB
		MaxFileBytes:  8 << 20,  // 8 MiB
		MaxEntries:    20000,
	}
}

// errUnsafeArchive is the class of failure that means the archive tried to
// write somewhere it was not invited.
var errUnsafeArchive = errors.New("unsafe archive")

// extractSubtree writes the `subtree` directory out of a GitHub source tarball
// into dest, and refuses anything else the archive contains.
//
// This function is the only place in sal where bytes from the network become
// files on disk, so it is deliberately suspicious. Three rules, each of which
// closes a documented way tar extraction gets exploited:
//
//  1. Only regular files and directories are extracted. A symlink in a source
//     tarball is a link that can be made to point outside dest, and a hardlink
//     is worse — so rather than trying to decide which ones are safe, every
//     other entry type is a hard failure. The bank contains no links, so
//     nothing legitimate is lost, and if it ever does the refusal is loud.
//  2. Every path is resolved and checked to still be inside dest. This is the
//     ../../../etc/whatever case, and the check is done on the joined absolute
//     path rather than on the string, because "a/../../b" looks fine until it
//     is resolved.
//  3. Modes come from this code, not from the archive. An archive that ships a
//     setuid bit, or a world-writable directory, does not get to.
func extractSubtree(r io.Reader, subtree, dest string, lim Limits) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("reading archive: %w", err)
	}
	defer gz.Close()

	// Bound the decompressed stream as a whole, so a small download cannot
	// expand into an unbounded write.
	tr := tar.NewReader(io.LimitReader(gz, lim.MaxTotalBytes+1))

	var (
		entries int
		written int64
		files   int
	)

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return files, fmt.Errorf("reading archive: %w", err)
		}

		entries++
		if entries > lim.MaxEntries {
			return files, fmt.Errorf("%w: more than %d entries", errUnsafeArchive, lim.MaxEntries)
		}

		// pax/global headers carry metadata, not content.
		if h.Typeflag == tar.TypeXGlobalHeader || h.Typeflag == tar.TypeXHeader {
			continue
		}

		rel, ok := relativeTo(subtree, h.Name)
		if !ok {
			continue // outside the subtree we asked for
		}

		switch h.Typeflag {
		case tar.TypeDir, tar.TypeReg:
			// The only two shapes a bank entry takes.
		default:
			return files, fmt.Errorf(
				"%w: %q is a %s; the bank contains only regular files and directories, and a link in a source archive is a way out of the directory it is extracted into",
				errUnsafeArchive, h.Name, typeName(h.Typeflag))
		}

		target, err := safeJoin(dest, rel)
		if err != nil {
			return files, err
		}

		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return files, err
			}
			continue
		}

		if h.Size > lim.MaxFileBytes {
			return files, fmt.Errorf("%w: %q is %d bytes, over the %d limit", errUnsafeArchive, h.Name, h.Size, lim.MaxFileBytes)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return files, err
		}

		n, err := writeFile(target, tr, lim.MaxFileBytes)
		if err != nil {
			return files, err
		}
		written += n
		files++

		if written > lim.MaxTotalBytes {
			return files, fmt.Errorf("%w: archive expands past %d bytes", errUnsafeArchive, lim.MaxTotalBytes)
		}
	}

	if files == 0 {
		return 0, fmt.Errorf("archive contains no %s/ directory", subtree)
	}
	return files, nil
}

// relativeTo strips the tarball's single root directory and then the subtree
// prefix, reporting whether the entry is inside the subtree at all.
//
// GitHub source tarballs wrap everything in one directory named for the repo
// and ref, whose exact spelling is not worth depending on — so the first path
// component is dropped positionally rather than matched.
func relativeTo(subtree, name string) (string, bool) {
	name = path.Clean(strings.TrimPrefix(name, "./"))
	if name == "." || name == "/" {
		return "", false
	}

	_, rest, found := strings.Cut(name, "/")
	if !found {
		return "", false // the root directory entry itself
	}
	if rest == subtree {
		return "", false // the subtree's own directory entry; nothing to write
	}
	rel, found := strings.CutPrefix(rest, subtree+"/")
	if !found || rel == "" {
		return "", false
	}
	return rel, true
}

// safeJoin resolves rel against dest and refuses anything that escapes it.
func safeJoin(dest, rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: path contains a NUL byte", errUnsafeArchive)
	}
	// A rooted path in the archive must never be honoured as rooted on disk.
	if path.IsAbs(rel) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: absolute path %q", errUnsafeArchive, rel)
	}

	target := filepath.Join(dest, filepath.FromSlash(rel))

	// Check the resolved path, not the string: "a/../../b" only reveals itself
	// once joined and cleaned.
	prefix := filepath.Clean(dest) + string(os.PathSeparator)
	if !strings.HasPrefix(target, prefix) {
		return "", fmt.Errorf("%w: %q escapes the extraction directory", errUnsafeArchive, rel)
	}
	return target, nil
}

// writeFile writes one entry, capped, with a mode this code chose.
func writeFile(target string, r io.Reader, max int64) (int64, error) {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// max+1 so that hitting exactly the limit is distinguishable from
	// exceeding it, and a lying header cannot write more than the cap.
	n, err := io.Copy(f, io.LimitReader(r, max+1))
	if err != nil {
		return n, err
	}
	if n > max {
		return n, fmt.Errorf("%w: %q exceeds %d bytes", errUnsafeArchive, target, max)
	}
	return n, f.Close()
}

func typeName(flag byte) string {
	switch flag {
	case tar.TypeSymlink:
		return "symlink"
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar:
		return "character device"
	case tar.TypeBlock:
		return "block device"
	case tar.TypeFifo:
		return "FIFO"
	}
	return fmt.Sprintf("tar type %q", flag)
}
