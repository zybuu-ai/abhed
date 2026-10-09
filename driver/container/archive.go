package container

import (
	"archive/tar"
	"errors"
	"io"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/archive"
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
		name, err := archive.Name(h.Name)
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
			if err := archive.Link(name, h.Linkname); err != nil {
				return err
			}
			out.Typeflag, out.Linkname = tar.TypeSymlink, h.Linkname
		case tar.TypeLink:
			target, err := archive.Name(h.Linkname)
			if err != nil {
				return driver.Errorf(driver.ErrInvalid, "hard link %s: %v", name, err)
			}
			out.Typeflag, out.Linkname = tar.TypeLink, target
		default:
			return driver.Errorf(driver.ErrInvalid, "archive entry %s is a %s, which is not copied", name, archive.TypeName(h.Typeflag))
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
