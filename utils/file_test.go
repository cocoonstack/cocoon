package utils

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestEnsureDirs_CreateNew(t *testing.T) {
	base := t.TempDir()
	dirs := []string{
		filepath.Join(base, "a"),
		filepath.Join(base, "b", "c"),
	}

	if err := EnsureDirs(dirs...); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	for _, d := range dirs {
		fi, err := os.Stat(d)
		if err != nil {
			t.Errorf("stat %s: %v", d, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("%s is not a directory", d)
		}
	}
}

func TestEnsureDirs_AlreadyExist(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureDirs(dir); err != nil {
		t.Fatalf("EnsureDirs on existing dir: %v", err)
	}
}

func TestEnsureDirs_Empty(t *testing.T) {
	if err := EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs with no args: %v", err)
	}
}

func TestEnsureDirs_FailsUnderFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "regular_file")
	os.WriteFile(file, []byte("x"), 0o644)

	err := EnsureDirs(filepath.Join(file, "subdir"))
	if err == nil {
		t.Fatal("expected error when creating dir under a file")
	}
}

func TestValidFile(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) string
		want  bool
	}{
		{"regular file", func(t *testing.T) string { return tempFile(t, "data") }, true},
		{"empty file", func(t *testing.T) string { return tempFile(t, "") }, false},
		{"directory", func(t *testing.T) string { return t.TempDir() }, false},
		{"nonexistent", func(*testing.T) string { return "/nonexistent/file" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidFile(tt.setup(t)); got != tt.want {
				t.Errorf("ValidFile = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScanFileStems_Basic(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"abc.erofs", "def.erofs", "readme.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "skip.erofs"), 0o755); err != nil {
		t.Fatal(err)
	}

	stems, err := ScanFileStems(dir, ".erofs")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(stems)
	if want := []string{"abc", "def"}; !slices.Equal(stems, want) {
		t.Errorf("got %v, want %v", stems, want)
	}
}

func TestScanFileStems_NoMatch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644)

	stems, err := ScanFileStems(dir, ".erofs")
	if err != nil {
		t.Fatal(err)
	}
	if len(stems) != 0 {
		t.Errorf("expected empty, got %v", stems)
	}
}

func TestScan_ErrorsAndMissing(t *testing.T) {
	scans := []struct {
		name string
		scan func(string) ([]string, error)
	}{
		{"stems", func(dir string) ([]string, error) { return ScanFileStems(dir, ".erofs") }},
		{"subdirs", ScanSubdirs},
	}
	for _, tt := range scans {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.scan(tempFile(t, "x")); err == nil {
				t.Fatal("expected error when scanning a file as directory")
			}
			got, err := tt.scan("/nonexistent/dir")
			if err != nil {
				t.Fatal(err)
			}
			if got != nil {
				t.Errorf("expected nil, got %v", got)
			}
		})
	}
}

func TestScanSubdirs_Basic(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "sub1"), 0o755)
	os.Mkdir(filepath.Join(dir, "sub2"), 0o755)
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644)

	subs, err := ScanSubdirs(dir)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(subs)
	if want := []string{"sub1", "sub2"}; !slices.Equal(subs, want) {
		t.Errorf("got %v, want %v", subs, want)
	}
}

func TestScanSubdirs_Empty(t *testing.T) {
	dir := t.TempDir()
	subs, err := ScanSubdirs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 0 {
		t.Errorf("expected empty, got %v", subs)
	}
}

func TestFilterUnreferenced(t *testing.T) {
	set := func(keys ...string) map[string]struct{} {
		m := make(map[string]struct{}, len(keys))
		for _, k := range keys {
			m[k] = struct{}{}
		}
		return m
	}
	tests := []struct {
		name       string
		candidates []string
		refs       map[string]struct{}
		excludes   []map[string]struct{}
		want       []string
	}{
		{"basic", []string{"a", "b", "c", "d"}, set("a", "c"), nil, []string{"b", "d"}},
		{"with exclude", []string{"a", "b", "c", "d"}, set("a"), []map[string]struct{}{set("b")}, []string{"c", "d"}},
		{"all referenced", []string{"a", "b"}, set("a", "b"), nil, nil},
		{"none referenced", []string{"x", "y"}, set(), nil, []string{"x", "y"}},
		{"empty", nil, nil, nil, nil},
		{"multiple exclude sets", []string{"a", "b", "c", "d", "e"}, set("a"), []map[string]struct{}{set("b"), set("c")}, []string{"d", "e"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterUnreferenced(tt.candidates, tt.refs, tt.excludes...)
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRemoveMatching(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		match   func(os.DirEntry) bool
		want    []string
	}{
		{
			name:    "removes matching files",
			entries: []string{"keep.txt", "remove.tmp", "also.tmp"},
			match:   func(e os.DirEntry) bool { return filepath.Ext(e.Name()) == ".tmp" },
			want:    []string{"keep.txt"},
		},
		{
			name:    "no matches",
			entries: []string{"file.txt"},
			match:   func(os.DirEntry) bool { return false },
			want:    []string{"file.txt"},
		},
		{
			name:    "removes subdirs",
			entries: []string{"subdir/", "subdir/inner.txt"},
			match:   func(e os.DirEntry) bool { return e.IsDir() },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, tt.entries...)

			if errs := RemoveMatching(t.Context(), dir, tt.match); len(errs) != 0 {
				t.Errorf("unexpected errors: %v", errs)
			}
			if got := dirNames(t, dir); !slices.Equal(got, tt.want) {
				t.Errorf("remaining: %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRemoveMatching_NonexistentDir(t *testing.T) {
	errs := RemoveMatching(t.Context(), "/nonexistent/dir", func(_ os.DirEntry) bool {
		return true
	})
	if len(errs) != 0 {
		t.Errorf("expected no errors for nonexistent dir, got %v", errs)
	}
}

func TestRemoveMatching_RemoveAllError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a read-only directory's contents")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "protected")
	os.Mkdir(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "inner.txt"), []byte("x"), 0o644)
	os.Chmod(sub, 0o444)
	t.Cleanup(func() { os.Chmod(sub, 0o755) })

	errs := RemoveMatching(t.Context(), dir, func(e os.DirEntry) bool {
		return e.Name() == "protected"
	})

	if len(errs) != 1 {
		t.Fatalf("errs = %v, want one removal error", errs)
	}
	if _, err := os.Stat(sub); err != nil {
		t.Fatalf("protected dir removed: %v", err)
	}
}

func TestDirSize_CountsAllocatedBytes(t *testing.T) {
	const apparent = 64 << 20
	dir := t.TempDir()
	path := filepath.Join(dir, "cow.raw")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(apparent); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(make([]byte, 4096), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	allocated := fi.Sys().(*syscall.Stat_t).Blocks * 512
	if allocated >= apparent/2 {
		t.Skipf("filesystem allocated %d of %d bytes, no hole support", allocated, apparent)
	}

	got, err := DirSize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != allocated {
		t.Errorf("DirSize = %d, want the %d allocated bytes, not the %d apparent", got, allocated, int64(apparent))
	}
}

func readFull(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func tempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTree(t *testing.T, dir string, entries ...string) {
	t.Helper()
	for _, e := range entries {
		path := filepath.Join(dir, e)
		if strings.HasSuffix(e, "/") {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}
