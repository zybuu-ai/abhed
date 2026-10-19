package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// A small SFTP version 3 client: open, read, write, stat, rename and close,
// which is all ssh_put and ssh_get need. Version 3 is what OpenSSH's
// sftp-server speaks. Writing it here keeps a protocol this small from
// bringing a dependency the operator has to accept.

const (
	fxpInit     = 1
	fxpVersion  = 2
	fxpOpen     = 3
	fxpClose    = 4
	fxpRead     = 5
	fxpWrite    = 6
	fxpFstat    = 8
	fxpRemove   = 13
	fxpRename   = 18
	fxpStatus   = 101
	fxpHandle   = 102
	fxpData     = 103
	fxpAttrs    = 105
	fxpExtended = 200

	fxfRead  = 0x01
	fxfWrite = 0x02
	fxfCreat = 0x08
	fxfTrunc = 0x10
	fxfExcl  = 0x20

	attrSize   = 0x01
	attrUIDGID = 0x02
	attrPerms  = 0x04
	attrTimes  = 0x08
	attrExt    = 0x80000000

	fxOK  = 0
	fxEOF = 1

	// chunk is how much one read or write request carries; OpenSSH takes
	// up to 255 KiB, and 32 KiB is what every server accepts.
	chunk = 32 << 10
	// maxPacket bounds a reply read from the server.
	maxPacket = 256 << 10
)

// sftpClient talks to one sftp subsystem; one request at a time.
type sftpClient struct {
	session *ssh.Session
	w       io.WriteCloser
	r       io.Reader
	mu      sync.Mutex
	id      uint32
	ext     map[string]string
}

// statusError is a failure the server reported.
type statusError struct {
	code uint32
	msg  string
}

func (e *statusError) Error() string {
	names := map[uint32]string{2: "no such file", 3: "permission denied", 4: "failure",
		5: "bad message", 8: "operation unsupported"}
	name := names[e.code]
	if name == "" {
		name = fmt.Sprintf("status %d", e.code)
	}
	if e.msg != "" && e.msg != name {
		return name + ": " + e.msg
	}
	return name
}

// isNoSuchFile reports a status naming a missing file.
func isNoSuchFile(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.code == 2
}

// openSFTP starts the sftp subsystem on h. track is given the client before
// it is first used, so a transfer stopped while it starts can close it.
func (h *Host) openSFTP(ctx context.Context, track func(*sftpClient) bool) (*sftpClient, error) {
	client, err := h.connect(ctx)
	if err != nil {
		return nil, err
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open session on %s: %w", h.cfg.Name, err)
	}
	w, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	r, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	if err := session.RequestSubsystem("sftp"); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("%s does not offer SFTP: %w", h.cfg.Name, err)
	}
	c := &sftpClient{session: session, w: w, r: r, ext: map[string]string{}}
	if !track(c) {
		_ = c.Close()
		return nil, ctx.Err()
	}
	if err := c.init(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("start SFTP on %s: %w", h.cfg.Name, err)
	}
	return c, nil
}

func (c *sftpClient) Close() error {
	_ = c.w.Close()
	return c.session.Close()
}

// ---------------------------------------------------------------- encoding

type buf struct{ bytes.Buffer }

func (b *buf) u8(v byte)    { b.WriteByte(v) }
func (b *buf) u32(v uint32) { _ = binary.Write(&b.Buffer, binary.BigEndian, v) }
func (b *buf) u64(v uint64) { _ = binary.Write(&b.Buffer, binary.BigEndian, v) }
func (b *buf) str(s []byte) { b.u32(uint32(len(s))); b.Write(s) }

type reader struct {
	b   []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil || n < 0 || len(r.b) < n {
		r.err = errors.New("short SFTP reply")
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) u32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (r *reader) u64() uint64 {
	b := r.take(8)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

func (r *reader) str() []byte { return r.take(int(r.u32())) }

// attrs are the file attributes this client reads.
type attrs struct {
	size    uint64
	hasSize bool
}

func (r *reader) attrs() attrs {
	var a attrs
	flags := r.u32()
	if flags&attrSize != 0 {
		a.size, a.hasSize = r.u64(), true
	}
	if flags&attrUIDGID != 0 {
		r.u32()
		r.u32()
	}
	if flags&attrPerms != 0 {
		r.u32()
	}
	if flags&attrTimes != 0 {
		r.u32()
		r.u32()
	}
	if flags&attrExt != 0 {
		for n := r.u32(); n > 0 && r.err == nil; n-- {
			r.str()
			r.str()
		}
	}
	return a
}

// ---------------------------------------------------------------- transport

func (c *sftpClient) send(typ byte, body []byte) error {
	var b buf
	b.u32(uint32(len(body) + 1))
	b.u8(typ)
	b.Write(body)
	_, err := c.w.Write(b.Bytes())
	return err
}

func (c *sftpClient) recv() (byte, *reader, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, fmt.Errorf("read SFTP reply: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxPacket {
		return 0, nil, fmt.Errorf("SFTP reply of %d bytes is out of bounds", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c.r, b); err != nil {
		return 0, nil, fmt.Errorf("read SFTP reply: %w", err)
	}
	return b[0], &reader{b: b[1:]}, nil
}

func (c *sftpClient) init() error {
	var b buf
	b.u32(3)
	if err := c.send(fxpInit, b.Bytes()); err != nil {
		return err
	}
	typ, r, err := c.recv()
	if err != nil {
		return err
	}
	if typ != fxpVersion {
		return fmt.Errorf("unexpected SFTP reply %d to init", typ)
	}
	if v := r.u32(); v != 3 || r.err != nil {
		return fmt.Errorf("the server speaks SFTP version %d, not 3", v)
	}
	for len(r.b) > 0 && r.err == nil {
		name, data := r.str(), r.str()
		c.ext[string(name)] = string(data)
	}
	return nil
}

// call sends one request with a fresh id and returns its reply.
func (c *sftpClient) call(typ byte, fill func(b *buf)) (byte, *reader, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.id++
	id := c.id
	var b buf
	b.u32(id)
	fill(&b)
	if err := c.send(typ, b.Bytes()); err != nil {
		return 0, nil, err
	}
	rt, r, err := c.recv()
	if err != nil {
		return 0, nil, err
	}
	if got := r.u32(); got != id || r.err != nil {
		return 0, nil, fmt.Errorf("SFTP reply for request %d, want %d", got, id)
	}
	if rt == fxpStatus {
		code, msg := r.u32(), r.str()
		if code == fxOK {
			return rt, r, nil
		}
		return rt, r, &statusError{code: code, msg: string(msg)}
	}
	return rt, r, nil
}

func (c *sftpClient) status(typ byte, fill func(b *buf)) error {
	rt, _, err := c.call(typ, fill)
	if err == nil && rt != fxpStatus {
		return fmt.Errorf("unexpected SFTP reply %d", rt)
	}
	return err
}

// ---------------------------------------------------------------- operations

func (c *sftpClient) open(path string, flags, perms uint32) ([]byte, error) {
	rt, r, err := c.call(fxpOpen, func(b *buf) {
		b.str([]byte(path))
		b.u32(flags)
		if perms != 0 {
			b.u32(attrPerms)
			b.u32(perms)
		} else {
			b.u32(0)
		}
	})
	if err != nil {
		return nil, err
	}
	if rt != fxpHandle {
		return nil, fmt.Errorf("unexpected SFTP reply %d to open", rt)
	}
	h := r.str()
	return append([]byte(nil), h...), r.err
}

func (c *sftpClient) close(handle []byte) error {
	return c.status(fxpClose, func(b *buf) { b.str(handle) })
}

func (c *sftpClient) fstat(handle []byte) (attrs, error) {
	return c.attrsOf(fxpFstat, func(b *buf) { b.str(handle) })
}

func (c *sftpClient) attrsOf(typ byte, fill func(b *buf)) (attrs, error) {
	rt, r, err := c.call(typ, fill)
	if err != nil {
		return attrs{}, err
	}
	if rt != fxpAttrs {
		return attrs{}, fmt.Errorf("unexpected SFTP reply %d to stat", rt)
	}
	a := r.attrs()
	return a, r.err
}

func (c *sftpClient) remove(path string) error {
	return c.status(fxpRemove, func(b *buf) { b.str([]byte(path)) })
}

// rename replaces newPath with oldPath: atomically where the server offers
// posix-rename, and otherwise only where newPath does not exist.
func (c *sftpClient) rename(oldPath, newPath string) error {
	if _, ok := c.ext["posix-rename@openssh.com"]; ok {
		return c.status(fxpExtended, func(b *buf) {
			b.str([]byte("posix-rename@openssh.com"))
			b.str([]byte(oldPath))
			b.str([]byte(newPath))
		})
	}
	return c.status(fxpRename, func(b *buf) {
		b.str([]byte(oldPath))
		b.str([]byte(newPath))
	})
}

// writeAll writes data through handle from offset 0.
func (c *sftpClient) writeAll(ctx context.Context, handle, data []byte) error {
	for off := 0; off < len(data); off += chunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(off+chunk, len(data))
		if err := c.status(fxpWrite, func(b *buf) {
			b.str(handle)
			b.u64(uint64(off))
			b.str(data[off:end])
		}); err != nil {
			return err
		}
	}
	return nil
}

// readAll reads through handle until the end of the file, refusing more
// than limit bytes.
func (c *sftpClient) readAll(ctx context.Context, handle []byte, limit int64) ([]byte, error) {
	var out bytes.Buffer
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		off := uint64(out.Len())
		rt, r, err := c.call(fxpRead, func(b *buf) {
			b.str(handle)
			b.u64(off)
			b.u32(chunk)
		})
		var se *statusError
		if errors.As(err, &se) && se.code == fxEOF {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
		if rt != fxpData {
			return nil, fmt.Errorf("unexpected SFTP reply %d to read", rt)
		}
		data := r.str()
		if r.err != nil {
			return nil, r.err
		}
		if len(data) == 0 {
			return out.Bytes(), nil
		}
		if int64(out.Len()+len(data)) > limit {
			return nil, fmt.Errorf("the file is larger than the %d bytes a transfer may carry", limit)
		}
		out.Write(data)
	}
}

// ---------------------------------------------------------------- transfers

// Put writes data to path on h with mode perms, through a file beside it
// that is renamed over it once complete, so a failed transfer leaves the old
// file. Where the server cannot rename over a file, it is written in place.
func (h *Host) Put(ctx context.Context, path string, data []byte, perms uint32) error {
	return h.inFlight(ctx, func(open func() (*sftpClient, error)) error {
		return h.put(ctx, open, path, data, perms)
	})
}

func (h *Host) put(ctx context.Context, open func() (*sftpClient, error), path string, data []byte, perms uint32) error {
	c, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	_, posix := c.ext["posix-rename@openssh.com"]
	target := path
	if posix {
		var n [6]byte
		_, _ = rand.Read(n[:])
		target = path + ".abhed-" + hex.EncodeToString(n[:])
	}
	flags := uint32(fxfWrite | fxfCreat | fxfTrunc)
	if posix {
		flags |= fxfExcl
	}
	handle, err := c.open(target, flags, perms)
	if err != nil {
		return fmt.Errorf("open %s on %s: %w", target, h.cfg.Name, err)
	}
	werr := c.writeAll(ctx, handle, data)
	var size attrs
	if werr == nil {
		size, werr = c.fstat(handle)
	}
	cerr := c.close(handle)
	if werr == nil && cerr != nil {
		werr = cerr
	}
	if werr == nil && size.hasSize && size.size != uint64(len(data)) {
		werr = fmt.Errorf("the host holds %d bytes of the %d sent", size.size, len(data))
	}
	if werr == nil && posix {
		werr = c.rename(target, path)
	}
	if werr != nil {
		if posix {
			_ = c.remove(target) // best effort: the transfer has already failed
		}
		return fmt.Errorf("write %s on %s: %w", path, h.cfg.Name, werr)
	}
	return nil
}

// Get reads path on h, refusing a file over limit bytes before reading it.
func (h *Host) Get(ctx context.Context, path string, limit int64) ([]byte, error) {
	var data []byte
	err := h.inFlight(ctx, func(open func() (*sftpClient, error)) error {
		var err error
		data, err = h.get(ctx, open, path, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (h *Host) get(ctx context.Context, open func() (*sftpClient, error), path string, limit int64) ([]byte, error) {
	c, err := open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	handle, err := c.open(path, fxfRead, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s on %s: %w", path, h.cfg.Name, err)
	}
	defer func() { _ = c.close(handle) }()
	if a, err := c.fstat(handle); err == nil && a.hasSize && a.size > uint64(limit) {
		return nil, fmt.Errorf("%s on %s is %d bytes, more than the %d a transfer may carry", path, h.cfg.Name, a.size, limit)
	}
	data, err := c.readAll(ctx, handle, limit)
	if err != nil {
		return nil, fmt.Errorf("read %s on %s: %w", path, h.cfg.Name, err)
	}
	return data, nil
}

// sftpGrace is how long a transfer stopped by its context has to end once
// its SFTP session is closed, before the connection to the host is closed too.
var sftpGrace = 2 * time.Second

// inFlight runs op until it ends or ctx does, whichever is first. When ctx
// ends first, the SFTP session is closed; if op is still blocked on the host
// after sftpGrace, so is the connection, so a host that stopped answering
// holds no transfer and no call waits on it.
func (h *Host) inFlight(ctx context.Context, op func(open func() (*sftpClient, error)) error) error {
	var mu sync.Mutex
	var live *sftpClient
	stopped := false
	track := func(c *sftpClient) bool {
		mu.Lock()
		defer mu.Unlock()
		live = c
		return !stopped
	}
	open := func() (*sftpClient, error) { return h.openSFTP(ctx, track) }
	done := make(chan error, 1)
	go func() { done <- op(open) }()
	select {
	case err := <-done:
		if err != nil && ctx.Err() != nil {
			return fmt.Errorf("transfer on %s stopped: %w", h.cfg.Name, ctx.Err())
		}
		return err
	case <-ctx.Done():
	}
	mu.Lock()
	stopped = true
	if live != nil {
		// The session only: closing its stdin would race a write in flight.
		_ = live.session.Close()
	}
	mu.Unlock()
	select {
	case <-done:
	case <-time.After(sftpGrace):
		_ = h.Close() // the next call dials again
	}
	return fmt.Errorf("transfer on %s stopped: %w", h.cfg.Name, ctx.Err())
}
