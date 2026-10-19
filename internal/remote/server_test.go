package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// sftpServerPaths are where OpenSSH's sftp-server is found.
var sftpServerPaths = []string{"/usr/libexec/sftp-server", "/usr/lib/openssh/sftp-server",
	"/usr/lib/ssh/sftp-server", "/usr/libexec/openssh/sftp-server"}

func sftpServerBinary() string {
	for _, p := range sftpServerPaths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// ioServer is startSSHServer's server that also reads stdin and serves the
// sftp subsystem with OpenSSH's own sftp-server, and keeps the commands it ran.
type ioServer struct {
	*testServer
	mu       sync.Mutex
	commands []string
	stdins   []string
}

func (s *ioServer) ran() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...), append([]string(nil), s.stdins...)
}

func startIOServer(t *testing.T, password string) *ioServer {
	t.Helper()
	srv := &ioServer{}
	signer, cfg := ioServerConfig(t, password)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.serve(conn, cfg)
		}
	}()
	srv.testServer = &testServer{addr: ln.Addr().String(), hostKey: signer.PublicKey(), stop: func() { _ = ln.Close() }}
	t.Cleanup(srv.stop)
	return srv
}

func ioServerConfig(t *testing.T, password string) (ssh.Signer, *ssh.ServerConfig) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
		if c.User() == "tester" && string(pass) == password {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(signer)
	return signer, cfg
}

func (s *ioServer) serve(nConn net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, requests, err := newChan.Accept()
		if err != nil {
			return
		}
		go s.session(ch, requests)
	}
}

func (s *ioServer) session(ch ssh.Channel, in <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	for req := range in {
		switch req.Type {
		case "subsystem":
			var p struct{ Name string }
			_ = ssh.Unmarshal(req.Payload, &p)
			bin := sftpServerBinary()
			if p.Name != "sftp" || bin == "" {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			cmd := exec.Command(bin, "-e", "-l", "QUIET")
			cmd.Stdin, cmd.Stdout = ch, ch
			_ = cmd.Run()
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			return
		case "exec":
			var p struct{ Command string }
			_ = ssh.Unmarshal(req.Payload, &p)
			_ = req.Reply(true, nil)
			var stdin []byte
			if strings.Contains(p.Command, "stdin") {
				stdin, _ = io.ReadAll(ch)
			}
			s.mu.Lock()
			s.commands, s.stdins = append(s.commands, p.Command), append(s.stdins, string(stdin))
			s.mu.Unlock()
			switch {
			case strings.HasPrefix(p.Command, "echo-stdin"):
				_, _ = ch.Write(stdin)
				_, _ = fmt.Fprintf(ch.Stderr(), "read %d bytes: %s", len(stdin), stdin)
			case strings.HasPrefix(p.Command, "hash-stdin"):
				sum := sha256.Sum256(stdin)
				_, _ = fmt.Fprintln(ch, hex.EncodeToString(sum[:]))
			default:
				_, _ = fmt.Fprintf(ch, "ran: %s\n", p.Command)
			}
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			return
		default:
			_ = req.Reply(false, nil)
		}
	}
}
