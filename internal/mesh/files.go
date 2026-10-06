package mesh

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const MaxBundleBytes = 32 << 20
const MaxWireBytes = 48 << 20

type File struct {
	Path       string `json:"path"`
	Data       []byte `json:"data"`
	SHA256     string `json:"sha256"`
	Executable bool   `json:"executable,omitempty"`
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func safeRelative(p string) bool {
	return p != "" && !strings.ContainsAny(p, "\\\x00\r\n") && !path.IsAbs(p) && path.Clean(p) == p && p != "." && p != ".." && !strings.HasPrefix(p, "../")
}

func validateFiles(files []File) error {
	total := 0
	seen := map[string]bool{}
	for _, f := range files {
		if !safeRelative(f.Path) {
			return fmt.Errorf("unsafe file path %q", f.Path)
		}
		if seen[f.Path] {
			return fmt.Errorf("duplicate input path %q", f.Path)
		}
		seen[f.Path] = true
		total += len(f.Data)
		if total > MaxBundleBytes {
			return fmt.Errorf("bundle exceeds %d MiB; use existing remote data for larger inputs", MaxBundleBytes>>20)
		}
		if digest(f.Data) != f.SHA256 {
			return fmt.Errorf("checksum mismatch for %s", f.Path)
		}
	}
	for name := range seen {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return fmt.Errorf("file/directory collision at %s", parent)
			}
		}
	}
	return nil
}

func gatherFiles(paths []string) ([]File, error) {
	var files []File
	total := int64(0)
	for _, source := range paths {
		source = filepath.Clean(source)
		info, e := os.Lstat(source)
		if e != nil {
			return nil, e
		}
		base := filepath.Base(source)
		e = filepath.WalkDir(source, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlinks are not transferred: %s", p)
			}
			if d.IsDir() {
				return nil
			}
			i, e := d.Info()
			if e != nil {
				return e
			}
			if !i.Mode().IsRegular() {
				return fmt.Errorf("not a regular file: %s", p)
			}
			total += i.Size()
			if total > MaxBundleBytes {
				return fmt.Errorf("bundle exceeds %d MiB", MaxBundleBytes>>20)
			}
			rel := base
			if info.IsDir() {
				r, e := filepath.Rel(source, p)
				if e != nil {
					return e
				}
				rel = filepath.Join(base, r)
			}
			b, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			files = append(files, File{filepath.ToSlash(rel), b, digest(b), i.Mode()&0111 != 0})
			return nil
		})
		if e != nil {
			return nil, e
		}
	}
	if e := validateFiles(files); e != nil {
		return nil, e
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func ensureNoSymlinks(root, relative string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	for p := root; ; p = filepath.Dir(p) {
		i, e := os.Lstat(p)
		if e == nil && i.Mode()&os.ModeSymlink != 0 {
			// macOS exposes these system directories through fixed aliases.
			if runtime.GOOS == "darwin" && (p == "/tmp" || p == "/var") {
				continue
			}
			return fmt.Errorf("symlink in destination: %s", p)
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	p := root
	for _, part := range strings.Split(relative, "/") {
		p = filepath.Join(p, part)
		i, e := os.Lstat(p)
		if e == nil && i.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in destination: %s", p)
		}
		if e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	return nil
}

func materialize(root string, files []File) error {
	if e := validateFiles(files); e != nil {
		return e
	}
	for _, f := range files {
		if e := ensureNoSymlinks(root, f.Path); e != nil {
			return e
		}
		dest := filepath.Join(root, filepath.FromSlash(f.Path))
		if old, e := os.ReadFile(dest); e == nil {
			if digest(old) == f.SHA256 {
				continue
			}
			return fmt.Errorf("refusing to overwrite existing file %s", dest)
		} else if !os.IsNotExist(e) {
			return e
		}
		if e := os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
			return e
		}
		mode := os.FileMode(0600)
		if f.Executable {
			mode = 0700
		}
		out, e := os.CreateTemp(filepath.Dir(dest), ".mesh-result-*")
		if e != nil {
			return e
		}
		temp := out.Name()
		if e = out.Chmod(mode); e == nil {
			_, e = out.Write(f.Data)
		}
		if e == nil {
			e = out.Sync()
		}
		ce := out.Close()
		if e != nil {
			os.Remove(temp)
			return e
		}
		if ce != nil {
			os.Remove(temp)
			return ce
		}
		// Link publishes complete bytes atomically and cannot replace an existing
		// file. Interrupted deliveries can be retried without partial artifacts.
		e = os.Link(temp, dest)
		_ = os.Remove(temp)
		if e != nil {
			return e
		}
	}
	return nil
}

func outputFiles(root string) ([]File, error) {
	entries, e := os.ReadDir(root)
	if e != nil {
		return nil, e
	}
	var paths []string
	for _, x := range entries {
		paths = append(paths, filepath.Join(root, x.Name()))
	}
	return gatherFiles(paths)
}
