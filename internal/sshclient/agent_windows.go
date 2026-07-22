//go:build windows

package sshclient

import (
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"dterm/internal/dlog"
)

func agentAuthMethod() ssh.AuthMethod {
	// Windows OpenSSH agent exposes a named pipe instead of a Unix socket.
	f, err := os.OpenFile(`\\.\pipe\openssh-ssh-agent`, os.O_RDWR, 0)
	if err != nil {
		dlog.Log("agent: named pipe open failed: %v", err)
		return nil
	}
	dlog.Log("agent: connected via named pipe")
	return ssh.PublicKeysCallback(agent.NewClient(f).Signers)
}
