//go:build !windows

package sshclient

import (
	"net"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"dterm/internal/dlog"
)

func agentAuthMethod() ssh.AuthMethod {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		dlog.Log("agent: SSH_AUTH_SOCK not set")
		return nil
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		dlog.Log("agent: unix socket dial failed: %v", err)
		return nil
	}
	dlog.Log("agent: connected via SSH_AUTH_SOCK")
	c := agent.NewClient(conn)
	return ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
		defer conn.Close()
		return c.Signers()
	})
}
