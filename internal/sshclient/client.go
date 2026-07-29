package sshclient

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"dterm/internal/config"
	"dterm/internal/dlog"
)

type Session struct {
	client  *ssh.Client
	session *ssh.Session
	stdin   io.WriteCloser
	stdout  io.Reader
	mu      sync.Mutex
	closed  bool

	rdOnce  sync.Once
	rdCh    chan []byte
	rdErrCh chan error
}

// startRead lazily starts a background goroutine that reads continuously from
// stdout and pushes chunks to rdCh. Called once per session.
func (s *Session) startRead() {
	s.rdOnce.Do(func() {
		s.rdCh = make(chan []byte, 64)
		s.rdErrCh = make(chan error, 1)
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := s.stdout.Read(buf)
				if n > 0 {
					chunk := make([]byte, n)
					copy(chunk, buf[:n])
					s.rdCh <- chunk
				}
				if err != nil {
					s.rdErrCh <- err
					return
				}
			}
		}()
	})
}

// ReadBatch blocks until at least one chunk of data arrives, then coalesces
// additional data for up to 2ms. This batches SSH output into fewer, larger
// messages so bubbletea re-renders less frequently during high-throughput output.
func (s *Session) ReadBatch() ([]byte, error) {
	s.startRead()
	var acc []byte

	// Wait for first chunk or error.
	select {
	case chunk := <-s.rdCh:
		acc = chunk
	case err := <-s.rdErrCh:
		return nil, err
	}

	// Coalesce: keep reading for up to 2ms or 256KB.
	timer := time.NewTimer(2 * time.Millisecond)
	defer timer.Stop()
	for len(acc) < 256*1024 {
		select {
		case chunk := <-s.rdCh:
			acc = append(acc, chunk...)
		case <-timer.C:
			return acc, nil
		}
	}
	return acc, nil
}

// expandPath handles both ~ and $ENV substitution.
func expandPath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[1:])
	}
	return os.ExpandEnv(path)
}

// loadKey reads and parses a private key, trying passphrase if the key is encrypted.
func loadKey(path, passphrase string) ssh.Signer {
	data, err := os.ReadFile(path)
	if err != nil {
		dlog.Log("key: cannot read %s: %v", path, err)
		return nil
	}

	signer, err := ssh.ParsePrivateKey(data)
	if err == nil {
		dlog.Log("key: loaded %s (no passphrase)", path)
		return signer
	}

	var missingErr *ssh.PassphraseMissingError
	if errors.As(err, &missingErr) {
		if passphrase == "" {
			dlog.Log("key: %s is passphrase-protected but no passphrase provided", path)
			return nil
		}
		signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
		if err != nil {
			dlog.Log("key: wrong passphrase for %s: %v", path, err)
			return nil
		}
		dlog.Log("key: loaded %s (with passphrase)", path)
		return signer
	}

	dlog.Log("key: parse error for %s: %v", path, err)
	return nil
}

var tofuMu sync.Mutex

// tofuCallback returns a HostKeyCallback that implements Trust-On-First-Use:
// unknown hosts are added to ~/.dterm/known_hosts; key mismatches are rejected.
func tofuCallback() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}
	khFile := filepath.Join(home, ".dterm", "known_hosts")
	if err := os.MkdirAll(filepath.Dir(khFile), 0700); err != nil {
		return nil, fmt.Errorf("known_hosts dir: %w", err)
	}
	// Create file if absent so knownhosts.New doesn't fail.
	f, err := os.OpenFile(khFile, os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	f.Close()

	cb, err := knownhosts.New(khFile)
	if err != nil {
		return nil, fmt.Errorf("known_hosts parse: %w", err)
	}

	return func(host string, remote net.Addr, key ssh.PublicKey) error {
		err := cb(host, remote, key)
		if err == nil {
			dlog.Log("hostkey: %s verified", host)
			return nil
		}
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return err
		}
		if len(keyErr.Want) > 0 {
			dlog.Log("hostkey: MISMATCH for %s", host)
			return fmt.Errorf("host key mismatch for %s — possible MITM: %w", host, err)
		}
		// Unknown host — add it (TOFU).
		tofuMu.Lock()
		defer tofuMu.Unlock()
		line := knownhosts.Line([]string{knownhosts.Normalize(host)}, key)
		wf, err := os.OpenFile(khFile, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("known_hosts write: %w", err)
		}
		defer wf.Close()
		if _, err := fmt.Fprintln(wf, line); err != nil {
			return fmt.Errorf("known_hosts write: %w", err)
		}
		dlog.Log("hostkey: trusted new host %s, saved to known_hosts", host)
		return nil
	}, nil
}

func Connect(bm config.Bookmark) (*Session, error) {
	dlog.Log("connect: bookmark=%s user=%s host=%s port=%d keyfile=%q",
		bm.Name, bm.User, bm.Host, bm.Port, bm.KeyFile)

	authMethods := []ssh.AuthMethod{}

	// SSH agent (platform-specific: Unix socket or Windows named pipe)
	if m := agentAuthMethod(); m != nil {
		dlog.Log("auth: added ssh-agent")
		authMethods = append(authMethods, m)
	} else {
		dlog.Log("auth: ssh-agent not available")
	}

	// explicit key file
	if bm.KeyFile != "" {
		expanded := expandPath(bm.KeyFile)
		dlog.Log("auth: trying explicit key_file %q (expanded: %q)", bm.KeyFile, expanded)
		if signer := loadKey(expanded, bm.Password); signer != nil {
			authMethods = append(authMethods, ssh.PublicKeys(signer))
		}
	} else {
		// Offer all loadable default keys; server picks the one it accepts.
		home, _ := os.UserHomeDir()
		var signers []ssh.Signer
		for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa", "id_dsa"} {
			path := filepath.Join(home, ".ssh", name)
			if signer := loadKey(path, bm.Password); signer != nil {
				dlog.Log("auth: queued default key %s", path)
				signers = append(signers, signer)
			}
		}
		if len(signers) > 0 {
			authMethods = append(authMethods, ssh.PublicKeys(signers...))
		}
	}

	// password auth
	if bm.Password != "" {
		dlog.Log("auth: added password auth")
		authMethods = append(authMethods, ssh.Password(bm.Password))
	}

	dlog.Log("auth: %d method(s) available", len(authMethods))
	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no auth method available for %s", bm.Name)
	}

	hkCB, err := tofuCallback()
	if err != nil {
		dlog.Log("hostkey: TOFU setup failed (%v), rejecting connection", err)
		return nil, fmt.Errorf("host key setup: %w", err)
	}

	cfg := &ssh.ClientConfig{
		User:            bm.User,
		Auth:            authMethods,
		HostKeyCallback: hkCB,
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(bm.Host, strconv.Itoa(bm.Port))
	dlog.Log("dial: %s", addr)
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		dlog.Log("dial error: %v", err)
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	dlog.Log("dial: connected")

	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("session: %w", err)
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 115200,
		ssh.TTY_OP_OSPEED: 115200,
	}

	if err := session.RequestPty("xterm-256color", 40, 120, modes); err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("pty: %w", err)
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, err
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, err
	}

	if err := session.Shell(); err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("shell: %w", err)
	}

	dlog.Log("session: shell started")
	return &Session{
		client:  client,
		session: session,
		stdin:   stdin,
		stdout:  stdout,
	}, nil
}

func (s *Session) Read(buf []byte) (int, error) {
	return s.stdout.Read(buf)
}

func (s *Session) Client() *ssh.Client { return s.client }

func (s *Session) Resize(rows, cols int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.session.WindowChange(rows, cols)
}

func (s *Session) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, fmt.Errorf("session closed")
	}
	return s.stdin.Write(data)
}

func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	dlog.Log("session: closing")
	s.stdin.Close()
	s.session.Close()
	s.client.Close()
}
