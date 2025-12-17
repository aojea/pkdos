package files

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// MakeTar walks the source and writes a tarball to the writer
func MakeTar(srcPath string, writer io.Writer, excludeRegex *regexp.Regexp) error {
	absSrcPath, err := filepath.Abs(filepath.Clean(srcPath))
	if err != nil {
		return err
	}

	// Check if the source is a directory
	info, err := os.Stat(absSrcPath)
	if err != nil {
		return err
	}

	// If it's a directory, we use the directory itself as the base.
	// This means files inside will have paths relative to the directory,
	// effectively stripping the directory name from the tar archive.
	baseDir := absSrcPath
	if !info.IsDir() {
		// If it's a file, we use its parent as the base, preserving the filename.
		baseDir = filepath.Dir(absSrcPath)
	}

	tw := tar.NewWriter(writer)
	defer tw.Close() //nolint:errcheck

	return filepath.Walk(absSrcPath, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Rebase the path so it's relative to the upload root
		relPath, err := filepath.Rel(baseDir, file)
		if err != nil {
			return err
		}

		// If we are uploading a directory, the walk starts with the directory itself.
		// Its relative path is ".". We skip adding a tar entry for "." to avoid
		// messing with the destination root permissions or creating a "./" folder.
		if relPath == "." {
			return nil
		}

		if excludeRegex != nil && excludeRegex.MatchString(relPath) {
			// If it matches and is a directory, skip the whole tree
			if fi.IsDir() {
				return filepath.SkipDir
			}
			// If it's a file, just skip adding it
			return nil
		}

		// Create header
		header, err := tar.FileInfoHeader(fi, fi.Name())
		if err != nil {
			return err
		}

		header.Name = relPath

		// Ensure binaries are executable (simple heuristic: if we are uploading, preserve local mode)
		// header.Mode is already populated by FileInfoHeader from local file
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !fi.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close() //nolint:errcheck

		_, err = io.Copy(tw, f)
		return err
	})
}
