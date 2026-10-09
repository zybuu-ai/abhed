package container

import (
	"archive/tar"
	"errors"
	"io"
	"path"
	"strings"

	"github.com/zybuu-ai/abhed/driver"
)

// Limits on an archive copied in or out, which is untrusted either way.
const (
	maxArchiveEntries = 100000
	maxArchiveBytes   = 4 << 30
)

// recheck reads a tar stream and writes it again, refusing any entry that
// could land outside the folder it is unpacked into: an absolute name, a
// "..", a link pointing out, a device or a pipe. Owners become uid:gid and
// setuid, setgid and sticky bits are cleared. Nothing is written for an
// entry before it is checked, and a refusal ends the stream with an error.
func recheck(src io.Reader, dst io.Writer, uid, gid int) error {
	tr, tw := tar.NewReader(src), tar.NewWriter(dst)
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return tw.Close()
		}
		if err != nil {
			return driver.Errorf(driver.ErrInvalid, "reading the archive: %v", err)
		}
		if n >= maxArchiveEntries {
			return driver.Errorf(driver.ErrInvalid, "the archive has more than %d entries", maxArchiveEntries)
		}
		name, err := entryName(h.Name)
		if err != nil {
			return err
		}
		out := &tar.Header{Name: name, Mode: h.Mode &^ 0o7000, ModTime: h.ModTime, Uid: uid, Gid: gid, Format: tar.FormatPAX}
		switch h.Typeflag {
		case tar.TypeReg, 0: // 0 is the old flag for a regular file
			out.Typeflag, out.Size = tar.TypeReg, h.Size
		case tar.TypeDir:
			out.Typeflag = tar.TypeDir
		case tar.TypeSymlink:
			if err := linkInside(name, h.Linkname); err != nil {
				return err
			}
			out.Typeflag, out.Linkname = tar.TypeSymlink, h.Linkname
		case tar.TypeLink:
			target, err := entryName(h.Linkname)
			if err != nil {
				return driver.Errorf(driver.ErrInvalid, "hard link %s: %v", name, err)
			}
			out.Typeflag, out.Linkname = tar.TypeLink, target
		default:
			return driver.Errorf(driver.ErrInvalid, "archive entry %s is a %s, which is not copied", name, typeName(h.Typeflag))
		}
		total += out.Size
		if total > maxArchiveBytes {
			return driver.Errorf(driver.ErrInvalid, "the archive holds more than %d bytes", int64(maxArchiveBytes))
		}
		if err := tw.WriteHeader(out); err != nil {
			return driver.Errorf(driver.ErrInternal, "writing the archive: %v", err)
		}
		if out.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, tr, out.Size); err != nil {
				return driver.Errorf(driver.ErrInvalid, "reading %s from the archive: %v", name, err)
			}
		}
	}
}

// entryName is name cleaned, or an error when it is absolute or climbs out.
func entryName(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) || strings.Contains(name, `\`) {
		return "", driver.Errorf(driver.ErrInvalid, "archive entry %q is not a relative path", name)
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", driver.Errorf(driver.ErrInvalid, "archive entry %q climbs out of the folder", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", driver.Errorf(driver.ErrInvalid, "archive entry %q holds ..", name)
		}
	}
	return clean, nil
}

// linkInside refuses a symbolic link whose target is absolute or resolves
// above the folder the archive is unpacked into.
func linkInside(name, target string) error {
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsRune(target, 0) {
		return driver.Errorf(driver.ErrInvalid, "link %s points to %q, outside the folder", name, target)
	}
	joined := path.Clean(path.Join(path.Dir(name), target))
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return driver.Errorf(driver.ErrInvalid, "link %s points to %q, outside the folder", name, target)
	}
	return nil
}

func typeName(t byte) string {
	switch t {
	case tar.TypeChar:
		return "character device"
	case tar.TypeBlock:
		return "block device"
	case tar.TypeFifo:
		return "named pipe"
	}
	return "entry of type " + string(rune(t))
}
