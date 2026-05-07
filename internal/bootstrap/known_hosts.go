package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// KnownHosts wraps an OpenSSH known_hosts file with TOFU semantics.
//
//   - If the host is unknown, the user is prompted via HostKeyPrompt.
//     On accept, the key is appended to the file.
//   - If the host is known and the key matches, the connection proceeds.
//   - If the host is known but the key has changed, the connection is
//     rejected unconditionally — the user must remove the offending line
//     manually. This mirrors OpenSSH's behaviour.
type KnownHosts struct {
	path string
	mu   sync.Mutex
}

// DefaultKnownHosts returns a KnownHosts at
// $XDG_CONFIG_HOME/seabird/known_hosts, creating the parent dir.
func DefaultKnownHosts() (*KnownHosts, error) {
	cd, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return &KnownHosts{path: filepath.Join(cd, "seabird", "known_hosts")}, nil
}

// NewKnownHosts returns a KnownHosts backed by an explicit file path,
// useful in tests.
func NewKnownHosts(path string) *KnownHosts { return &KnownHosts{path: path} }

// Path returns the on-disk file backing this store.
func (k *KnownHosts) Path() string { return k.path }

func (k *KnownHosts) ensureFile() error {
	if err := os.MkdirAll(filepath.Dir(k.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(k.path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

func (k *KnownHosts) callback() (ssh.HostKeyCallback, error) {
	if err := k.ensureFile(); err != nil {
		return nil, err
	}
	return knownhosts.New(k.path)
}

func (k *KnownHosts) append(addr string, key ssh.PublicKey) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.ensureFile(); err != nil {
		return err
	}
	f, err := os.OpenFile(k.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line := knownhosts.Line([]string{knownhosts.Normalize(addr)}, key)
	if _, err := fmt.Fprintln(f, line); err != nil {
		return err
	}
	return nil
}

// hostKeyCallback returns the ssh.HostKeyCallback to use during Dial.
// It composes the known_hosts store with an interactive prompt for
// previously-unseen hosts.
func hostKeyCallback(ctx context.Context, store *KnownHosts, prompt HostKeyPrompt) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		// Best-effort: parse the existing file. If it's missing, treat the
		// host as unknown.
		cb, err := store.callback()
		if err == nil {
			if err := cb(hostname, remote, key); err == nil {
				return nil
			} else {
				var kerr *knownhosts.KeyError
				if errors.As(err, &kerr) {
					if len(kerr.Want) > 0 {
						return fmt.Errorf("host key mismatch for %s: refuse to connect, "+
							"remove the offending line from %s if you trust the new key",
							hostname, store.Path())
					}
					// fall through: unknown host -> prompt
				} else {
					return err
				}
			}
		}

		if prompt == nil {
			return fmt.Errorf("host %s is unknown and no prompt was provided", hostname)
		}
		dec, perr := prompt(ctx, hostname, key)
		if perr != nil {
			return perr
		}
		if dec != HostKeyAccept {
			return fmt.Errorf("host key for %s rejected by user", hostname)
		}
		if err := store.append(hostname, key); err != nil {
			return fmt.Errorf("persist host key: %w", err)
		}
		return nil
	}
}
