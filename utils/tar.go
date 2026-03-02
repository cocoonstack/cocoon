package utils

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// TarDir writes all regular files in dir into tw as flat tar entries (no directory nesting).
func TarDir(tw *tar.Writer, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", dir, err)
	}

	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		if err := TarFile(tw, filepath.Join(dir, entry.Name()), entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

// TarFile writes a single file into tw with the given name.
func TarFile(tw *tar.Writer, path, nameInTar string) error {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	hdr, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return fmt.Errorf("tar header for %s: %w", path, err)
	}
	hdr.Name = nameInTar

	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write header %s: %w", nameInTar, err)
	}

	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("write data %s: %w", nameInTar, err)
	}
	return nil
}

// ExtractTarGz decompresses a gzip stream, then extracts tar entries as flat
// files into dir. Only regular files are extracted; the base name is used to
// prevent path traversal.
func ExtractTarGz(dir string, r io.Reader) error {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gr.Close() //nolint:errcheck

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}

		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		name := filepath.Base(hdr.Name)
		if name == "." || name == ".." {
			continue
		}

		if err := extractFile(filepath.Join(dir, name), tr, hdr.FileInfo().Mode()); err != nil {
			return fmt.Errorf("extract %s: %w", name, err)
		}
	}
}

// extractFile creates a file at path and copies content from r.
func extractFile(path string, r io.Reader, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	return f.Sync()
}
