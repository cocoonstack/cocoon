package utils

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteFile_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.dat")
	data := []byte("hello atomic")

	if err := AtomicWriteFile(path, data, 0o644); err != nil {
		t.Fatalf("AtomicWriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello atomic" {
		t.Errorf("got %q, want %q", got, "hello atomic")
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("permissions: got %o, want 0644", fi.Mode().Perm())
	}
}

func TestAtomicWriteFile_Overwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overwrite.dat")

	if err := AtomicWriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("got %q, want %q", got, "new")
	}

	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("permissions: got %o, want 0600", fi.Mode().Perm())
	}
}

func TestAtomicWriteFile_EmptyData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.dat")

	if err := AtomicWriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Errorf("size: got %d, want 0", fi.Size())
	}
}

func TestAtomicWriteFile_BadDir(t *testing.T) {
	err := AtomicWriteFile("/nonexistent/dir/file.dat", []byte("x"), 0o644)
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

func TestAtomicWriteFile_NoTempLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clean.dat")

	if err := AtomicWriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected 1 file, got %d", len(entries))
	}
}

func TestAtomicWriteJSON_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	input := map[string]string{"key": "value"}
	if err := AtomicWriteJSON(path, input); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["key"] != "value" {
		t.Errorf("got %v", got)
	}

	if data[len(data)-1] != '\n' {
		t.Error("expected trailing newline")
	}
}

func TestAtomicWriteJSON_Struct(t *testing.T) {
	type Config struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	if err := AtomicWriteJSON(path, Config{Name: "test", Count: 42}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var got Config
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "test" || got.Count != 42 {
		t.Errorf("got %+v", got)
	}
}

func TestAtomicWriteJSON_Unmarshalable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")

	// Channels can't be marshaled.
	err := AtomicWriteJSON(path, make(chan int))
	if err == nil {
		t.Fatal("expected error for unmarshalable value")
	}
}

func TestAtomicWriteJSON_Permissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perm.json")

	if err := AtomicWriteJSON(path, "hello"); err != nil {
		t.Fatal(err)
	}

	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("permissions: got %o, want 0644", fi.Mode().Perm())
	}
}

func TestSyncParentDir_ValidDir(t *testing.T) {
	dir := t.TempDir()
	if err := SyncParentDir(dir); err != nil {
		t.Errorf("SyncParentDir: %v", err)
	}
}

func TestSyncParentDir_NonexistentDir(t *testing.T) {
	err := SyncParentDir("/nonexistent/dir")
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}
