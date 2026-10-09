// Package fileutil provides common filesystem helpers shared across tachi:
// atomic writes, JSON persistence, and existence checks.
package fileutil

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// MarshalJSON serializes v as JSON with 2-space indentation — the project-wide
// convention for persisted JSON files.
func MarshalJSON(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// ReadJSON reads and unmarshals a JSON file. The returned error can be checked
// with errors.Is(err, os.ErrNotExist) for missing files.
func ReadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// WriteFile writes data to path, creating parent directories as needed.
// dirPerm applies to any directories created, filePerm to the file.
func WriteFile(path string, data []byte, dirPerm, filePerm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return err
	}
	return os.WriteFile(path, data, filePerm)
}

// WriteFilePrivate writes data with private permissions (dir 0700, file 0600).
func WriteFilePrivate(path string, data []byte) error {
	return WriteFile(path, data, 0o700, 0o600)
}

// WriteFileShared writes data with shared permissions (dir 0755, file 0644).
func WriteFileShared(path string, data []byte) error {
	return WriteFile(path, data, 0o755, 0o644)
}

// AtomicWriteFile writes data to path atomically: it writes to a temporary
// file in the same directory, then renames it over path. On any failure the
// temporary file is removed. Parent directories are created as needed.
func AtomicWriteFile(path string, data []byte, dirPerm, filePerm os.FileMode) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, filePerm); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// AtomicWriteFilePrivate atomically writes data with private permissions
// (dir 0700, file 0600).
func AtomicWriteFilePrivate(path string, data []byte) error {
	return AtomicWriteFile(path, data, 0o700, 0o600)
}

// AtomicWriteFileShared atomically writes data with shared permissions
// (dir 0755, file 0644).
func AtomicWriteFileShared(path string, data []byte) error {
	return AtomicWriteFile(path, data, 0o755, 0o644)
}

// WriteJSON marshals v with 2-space indentation and writes it to path,
// creating parent directories as needed.
func WriteJSON(path string, v any, dirPerm, filePerm os.FileMode) error {
	data, err := MarshalJSON(v)
	if err != nil {
		return err
	}
	return WriteFile(path, data, dirPerm, filePerm)
}

// WriteJSONPrivate marshals v and writes it with private permissions
// (dir 0700, file 0600).
func WriteJSONPrivate(path string, v any) error {
	return WriteJSON(path, v, 0o700, 0o600)
}

// WriteJSONShared marshals v and writes it with shared permissions
// (dir 0755, file 0644).
func WriteJSONShared(path string, v any) error {
	return WriteJSON(path, v, 0o755, 0o644)
}

// AtomicWriteJSON marshals v with 2-space indentation and writes it to path
// atomically (tmp file + rename, temp cleaned up on failure).
func AtomicWriteJSON(path string, v any, dirPerm, filePerm os.FileMode) error {
	data, err := MarshalJSON(v)
	if err != nil {
		return err
	}
	return AtomicWriteFile(path, data, dirPerm, filePerm)
}

// AtomicWriteJSONPrivate marshals v and writes it atomically with private
// permissions (dir 0700, file 0600).
func AtomicWriteJSONPrivate(path string, v any) error {
	return AtomicWriteJSON(path, v, 0o700, 0o600)
}

// AtomicWriteJSONShared marshals v and writes it atomically with shared
// permissions (dir 0755, file 0644).
func AtomicWriteJSONShared(path string, v any) error {
	return AtomicWriteJSON(path, v, 0o755, 0o644)
}

// CanonicalPath returns the form of path every rule and every caller can agree
// on: absolute, cleaned, and with symlinks resolved as far as the path exists.
//
// Resolving the existing PREFIX is the whole point. A path whose leaf does not
// exist yet — a write that is about to create a file — still has an identity: the
// directory it will live in, which may itself be reached through a symlink. So the
// nearest existing ancestor is resolved and the remainder re-attached, and two
// aliases of the same to-be-created file agree instead of looking like two files.
//
// Callers use it to compare a path against a rule, or to key a path: what matters
// is that the same file always produces the same string, never that the file is
// there.
func CanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real), nil
	}
	dir, rest := filepath.Clean(abs), ""
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			// Walked up to the root without finding an existing ancestor: nothing
			// left to resolve, so the cleaned absolute path is the answer.
			return filepath.Clean(abs), nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Clean(filepath.Join(real, rest)), nil
		}
	}
}

// Exists reports whether a file or directory exists at path.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// RemoveIgnoreNotExist removes the file at path, treating a missing file as
// success. Other errors (permissions, I/O) are returned. Used for best-effort
// cleanup where a concurrently-removed file is not an error.
func RemoveIgnoreNotExist(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// IsSinglePathElement reports whether name is exactly one entry in one directory: no separator, no
// traversal, and not the current or parent directory itself.
//
// It is the rule for a name that arrives from OUTSIDE — a webview, a stored record — before it is
// joined onto a directory the program owns. A single shared implementation because the failure it
// prevents is the same everywhere: "../../other" joined onto a path escapes it, and the caller is
// usually one file write, or one `open`, away from acting on the result.
func IsSinglePathElement(name string) bool {
	return name != "" && name != "." && name != ".." &&
		name == filepath.Base(name) && !strings.ContainsAny(name, `/\`)
}

// IsDir reports whether path exists and is a directory.
func IsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// IsFile reports whether path exists and is a regular file.
func IsFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
