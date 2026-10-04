//go:build ignore

// Command tarball writes a directory as a reproducible gzipped tar: entries in lexical order, one modification time and no owner.
package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) != 4 {
		panic("usage: go run hack/tarball.go <directory> <archive> <unix seconds>")
	}
	if err := write(os.Args[1], os.Args[2], os.Args[3]); err != nil {
		panic(err)
	}
}

func write(dir, archive, epoch string) (err error) {
	secs, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil {
		return fmt.Errorf("modification time %q: %w", epoch, err)
	}
	mtime := time.Unix(secs, 0).UTC()
	out, err := os.OpenFile(filepath.Clean(archive), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	zw, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)
	root := filepath.Base(dir)
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: path.Join(root, filepath.ToSlash(rel)), ModTime: mtime, Format: tar.FormatUSTAR}
		switch {
		case d.IsDir():
			hdr.Typeflag, hdr.Name, hdr.Mode = tar.TypeDir, hdr.Name+"/", 0o755
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeReg, 0o444, info.Size()
		default:
			return fmt.Errorf("%s is neither a regular file nor a directory", p)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil
		}
		f, err := os.Open(filepath.Clean(p))
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		return errors.Join(err, f.Close())
	})
	return errors.Join(walkErr, tw.Close(), zw.Close())
}
