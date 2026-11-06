// Package console provides an interactive SSH session for the camera console.
// It does not invoke a local shell or keep passwords in files.
package console

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostKeyError is safe to display. It contains a public fingerprint, not a
// credential. Changed keys cannot be accepted with a first-use confirmation.
type HostKeyError struct {
	Fingerprint string `json:"fingerprint"`
	Algorithm   string `json:"algorithm"`
	Changed     bool   `json:"changed"`
}

func (e *HostKeyError) Error() string {
	if e.Changed {
		return "SSH-ключ камеры изменился. Сверьте ключ платы перед подключением."
	}
	return "Подтвердите SSH-ключ камеры при первом подключении."
}

type HostKeys struct {
	mu       sync.Mutex
	path     string
	external []string
}

// external files (for example ~/.ssh/known_hosts) are read-only. New explicitly
// trusted keys are written only to the application's data directory.
func NewHostKeys(path string, external ...string) *HostKeys {
	return &HostKeys{path: path, external: external}
}

func (s *HostKeys) loadLocked() (ssh.HostKeyCallback, error) {
	files := make([]string, 0, len(s.external)+1)
	for _, file := range append(append([]string{}, s.external...), s.path) {
		if _, err := os.Stat(file); err == nil {
			files = append(files, file)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("Не удалось прочитать сохранённые SSH-ключи")
		}
	}
	check, err := knownhosts.New(files...)
	if err != nil {
		return nil, errors.New("Не удалось прочитать формат known_hosts")
	}
	return check, nil
}

// KeyError.Want exposes the keys for this exact host, including hashed and
// wildcard known_hosts entries. The probe is never sent over the network.
type hostKeyProbe struct{}

func (hostKeyProbe) Type() string                        { return "dozor-host-key-lookup" }
func (hostKeyProbe) Marshal() []byte                     { return nil }
func (hostKeyProbe) Verify([]byte, *ssh.Signature) error { return errors.New("lookup only") }

// Prefer already trusted key types, as OpenSSH does. A camera can advertise
// ECDSA and Ed25519 simultaneously; choosing an unrecorded type first creates
// a false key-change warning even though a trusted type remains available.
func (s *HostKeys) algorithms(address string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	check, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	err = check(address, &net.TCPAddr{IP: net.IPv4zero, Port: 22}, hostKeyProbe{})
	var mismatch *knownhosts.KeyError
	var preferred []string
	if errors.As(err, &mismatch) {
		for _, known := range mismatch.Want {
			if known.Key.Type() == ssh.KeyAlgoRSA {
				preferred = append(preferred, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
			} else {
				preferred = append(preferred, known.Key.Type())
			}
		}
	}
	secure := ssh.SupportedAlgorithms().HostKeys
	allowed := make(map[string]bool, len(secure))
	for _, algorithm := range secure {
		allowed[algorithm] = true
	}
	seen := make(map[string]bool)
	var algorithms []string
	for _, algorithm := range append(preferred, secure...) {
		if allowed[algorithm] && !seen[algorithm] {
			algorithms = append(algorithms, algorithm)
			seen[algorithm] = true
		}
	}
	return algorithms, nil
}

func (s *HostKeys) callback(acceptedFingerprint string) ssh.HostKeyCallback {
	return func(host string, remote net.Addr, key ssh.PublicKey) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		check, err := s.loadLocked()
		if err != nil {
			return err
		}
		if err = check(host, remote, key); err == nil {
			return nil
		}
		var mismatch *knownhosts.KeyError
		if !errors.As(err, &mismatch) {
			return errors.New("SSH-ключ камеры отклонён")
		}
		fingerprint := ssh.FingerprintSHA256(key)
		if len(mismatch.Want) > 0 || acceptedFingerprint != fingerprint {
			return &HostKeyError{Fingerprint: fingerprint, Algorithm: key.Type(), Changed: len(mismatch.Want) > 0}
		}
		if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
			return errors.New("Не удалось создать хранилище SSH-ключей")
		}
		file, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return errors.New("Не удалось сохранить SSH-ключ камеры")
		}
		defer file.Close()
		if _, err = file.WriteString(knownhosts.Line([]string{host}, key) + "\n"); err != nil {
			return errors.New("Не удалось сохранить SSH-ключ камеры")
		}
		return file.Sync()
	}
}
