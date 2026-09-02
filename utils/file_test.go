package utils

import (
	"os"
	"path/filepath"
	"slices"
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
	os.WriteFile(file, []byte("x"), 0o644) //nolint:errcheck

	err := EnsureDirs(filepath.Join(file, "subdir"))
	if err == nil {
		t.Fatal("expected error when creating dir under a file")
	}
}

func TestValidFile_RegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ValidFile(path) {
		t.Error("expected ValidFile=true for regular file")
	}
}

func TestValidFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if ValidFile(path) {
		t.Error("expected ValidFile=false for empty file")
	}
}

func TestValidFile_Directory(t *testing.T) {
	if ValidFile(t.TempDir()) {
		t.Error("expected ValidFile=false for directory")
	}
}

func TestValidFile_Nonexistent(t *testing.T) {
	if ValidFile("/nonexistent/file") {
		t.Error("expected ValidFile=false for nonexistent file")
	}
}

func TestScanFileStems_Basic(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"abc.erofs", "def.erofs", "readme.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.Mkdir(filepath.Join(dir, "skip.erofs"), 0o755) //nolint:errcheck

	stems, err := ScanFileStems(dir, ".erofs")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(stems)
	if len(stems) != 2 || stems[0] != "abc" || stems[1] != "def" {
		t.Errorf("got %v, want [abc def]", stems)
	}
}

func TestScanFileStems_NoMatch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644) //nolint:errcheck

	stems, err := ScanFileStems(dir, ".erofs")
	if err != nil {
		t.Fatal(err)
	}
	if len(stems) != 0 {
		t.Errorf("expected empty, got %v", stems)
	}
}

func TestScanFileStems_ReadDirError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not_a_dir")
	os.WriteFile(file, []byte("x"), 0o644) //nolint:errcheck

	_, err := ScanFileStems(file, ".erofs")
	if err == nil {
		t.Fatal("expected error when scanning a file as directory")
	}
}

func TestScanFileStems_NonexistentDir(t *testing.T) {
	stems, err := ScanFileStems("/nonexistent/dir", ".erofs")
	if err != nil {
		t.Fatal(err)
	}
	if stems != nil {
		t.Errorf("expected nil, got %v", stems)
	}
}

func TestScanSubdirs_Basic(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "sub1"), 0o755)                      //nolint:errcheck
	os.Mkdir(filepath.Join(dir, "sub2"), 0o755)                      //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644) //nolint:errcheck

	subs, err := ScanSubdirs(dir)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(subs)
	if len(subs) != 2 || subs[0] != "sub1" || subs[1] != "sub2" {
		t.Errorf("got %v, want [sub1 sub2]", subs)
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

func TestScanSubdirs_ReadDirError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not_a_dir")
	os.WriteFile(file, []byte("x"), 0o644) //nolint:errcheck

	_, err := ScanSubdirs(file)
	if err == nil {
		t.Fatal("expected error when scanning a file as directory")
	}
}

func TestScanSubdirs_NonexistentDir(t *testing.T) {
	subs, err := ScanSubdirs("/nonexistent/dir")
	if err != nil {
		t.Fatal(err)
	}
	if subs != nil {
		t.Errorf("expected nil, got %v", subs)
	}
}

func TestFilterUnreferenced_Basic(t *testing.T) {
	candidates := []string{"a", "b", "c", "d"}
	refs := map[string]struct{}{"a": {}, "c": {}}

	got := FilterUnreferenced(candidates, refs)
	slices.Sort(got)
	if len(got) != 2 || got[0] != "b" || got[1] != "d" {
		t.Errorf("got %v, want [b d]", got)
	}
}

func TestFilterUnreferenced_WithExclude(t *testing.T) {
	candidates := []string{"a", "b", "c", "d"}
	refs := map[string]struct{}{"a": {}}
	exclude := map[string]struct{}{"b": {}}

	got := FilterUnreferenced(candidates, refs, exclude)
	slices.Sort(got)
	if len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Errorf("got %v, want [c d]", got)
	}
}

func TestFilterUnreferenced_AllReferenced(t *testing.T) {
	candidates := []string{"a", "b"}
	refs := map[string]struct{}{"a": {}, "b": {}}

	got := FilterUnreferenced(candidates, refs)
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestFilterUnreferenced_NoneReferenced(t *testing.T) {
	candidates := []string{"x", "y"}
	refs := map[string]struct{}{}

	got := FilterUnreferenced(candidates, refs)
	if len(got) != 2 {
		t.Errorf("got %v, want [x y]", got)
	}
}

func TestFilterUnreferenced_Empty(t *testing.T) {
	got := FilterUnreferenced(nil, nil)
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestFilterUnreferenced_MultipleExcludeSets(t *testing.T) {
	candidates := []string{"a", "b", "c", "d", "e"}
	refs := map[string]struct{}{"a": {}}
	ex1 := map[string]struct{}{"b": {}}
	ex2 := map[string]struct{}{"c": {}}

	got := FilterUnreferenced(candidates, refs, ex1, ex2)
	slices.Sort(got)
	if len(got) != 2 || got[0] != "d" || got[1] != "e" {
		t.Errorf("got %v, want [d e]", got)
	}
}

func TestRemoveMatching_Basic(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("k"), 0o644)   //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "remove.tmp"), []byte("r"), 0o644) //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "also.tmp"), []byte("r2"), 0o644)  //nolint:errcheck

	errs := RemoveMatching(t.Context(), dir, func(e os.DirEntry) bool {
		return filepath.Ext(e.Name()) == ".tmp"
	})
	if len(errs) != 0 {
		t.Errorf("unexpected errors: %v", errs)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "keep.txt" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("remaining: %v, want [keep.txt]", names)
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

func TestRemoveMatching_NoMatches(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644) //nolint:errcheck

	errs := RemoveMatching(t.Context(), dir, func(_ os.DirEntry) bool {
		return false
	})
	if len(errs) != 0 {
		t.Errorf("expected no errors, got %v", errs)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected 1 file, got %d", len(entries))
	}
}

func TestRemoveMatching_RemoveAllError(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "protected")
	os.Mkdir(sub, 0o755)                                              //nolint:errcheck
	os.WriteFile(filepath.Join(sub, "inner.txt"), []byte("x"), 0o644) //nolint:errcheck
	os.Chmod(sub, 0o444)                                              //nolint:errcheck
	t.Cleanup(func() { os.Chmod(sub, 0o755) })                        //nolint:errcheck

	errs := RemoveMatching(t.Context(), dir, func(e os.DirEntry) bool {
		return e.Name() == "protected"
	})

	_ = errs
}

func TestRemoveMatching_RemovesSubdirs(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "subdir")
	os.Mkdir(sub, 0o755)                                              //nolint:errcheck
	os.WriteFile(filepath.Join(sub, "inner.txt"), []byte("x"), 0o644) //nolint:errcheck

	errs := RemoveMatching(t.Context(), dir, func(e os.DirEntry) bool {
		return e.IsDir()
	})
	if len(errs) != 0 {
		t.Errorf("unexpected errors: %v", errs)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected empty dir, got %d entries", len(entries))
	}
}
