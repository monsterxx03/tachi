// Package fileutil provides common filesystem helpers shared across tachi:
// atomic writes, JSON persistence, and existence checks.
package fileutil

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
