// Package transport provides clipd's TLS layer: a self-signed server
// certificate that clients pin by public key.
//
// There is no certificate authority. Two machines with one owner do not need a
// chain of trust — they need to recognise each other, which is what pinning
// does. The model is an SSH host key rather than a web certificate: the server
// generates one long-lived keypair, the client records its fingerprint once
// during pairing, and any deviation afterwards aborts the connection.
package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultValidity is the lifetime of a generated certificate.
	//
	// Long, deliberately. Under pinning, reissuing a certificate with the same
	// key gains nothing (the key is the secret) and reissuing with a new key
	// breaks every client (the key is the identity), so there is no renewal
	// that is both meaningful and non-breaking. A short lifetime would buy a
	// re-pairing chore on a timer rather than any safety. Rotation is instead
	// a deliberate act: `clipd setup -rotate-cert`.
	DefaultValidity = 10 * 365 * 24 * time.Hour

	// ExpiryWarning is how much remaining validity `clipd status` starts
	// warning about, so an expiry is never a surprise.
	ExpiryWarning = 90 * 24 * time.Hour

	// FingerprintPrefix labels the hash algorithm so a future change is
	// unambiguous rather than silently reinterpreted.
	FingerprintPrefix = "sha256:"

	// FingerprintSize is the length of a SHA-256 digest.
	FingerprintSize = sha256.Size

	certPerm fs.FileMode = 0o644 // public
	keyPerm  fs.FileMode = 0o600 // private
	dirPerm  fs.FileMode = 0o700
)

var (
	// ErrNoCertificate means the peer completed a handshake without presenting
	// a certificate, which a pinning client cannot evaluate.
	ErrNoCertificate = errors.New("transport: server presented no certificate")

	// ErrNoPin means no fingerprint is configured, so there is nothing to
	// verify the server against.
	ErrNoPin = errors.New("transport: no server fingerprint configured")
)

// PinMismatchError reports a server whose key is not the pinned one. It is
// either a re-keyed server or an impersonation attempt; both deserve the full
// fingerprints so the user can tell which.
type PinMismatchError struct {
	Want []byte
	Got  []byte
}

func (e *PinMismatchError) Error() string {
	return fmt.Sprintf("server key fingerprint %s does not match the pinned %s",
		FormatFingerprint(e.Got), FormatFingerprint(e.Want))
}

// ValidityError reports a certificate outside its validity period.
type ValidityError struct {
	NotBefore time.Time
	NotAfter  time.Time
	Now       time.Time
}

func (e *ValidityError) Error() string {
	if e.Now.Before(e.NotBefore) {
		return fmt.Sprintf("server certificate is not valid until %s (check the clock on both machines)",
			e.NotBefore.Format(time.RFC3339))
	}
	return fmt.Sprintf("server certificate expired on %s (run `clipd setup -rotate-cert` on the server, then re-run `clipd configure -fingerprint`)",
		e.NotAfter.Format(time.RFC3339))
}

// GenerateCert creates a self-signed Ed25519 certificate.
//
// validity is a parameter rather than a constant so tests can produce expired
// and not-yet-valid certificates; production callers pass DefaultValidity.
func GenerateCert(validity time.Duration) (certPEM, keyPEM []byte, err error) {
	if validity <= 0 {
		return nil, nil, fmt.Errorf("transport: validity %s must be positive", validity)
	}
	now := time.Now()
	// NotBefore is backdated to absorb clock skew between the two machines,
	// which otherwise shows up as a baffling "not valid until" failure on a
	// fresh install.
	return generateCert(now.Add(-time.Hour), now.Add(validity))
}

// generateCert builds a certificate over an explicit validity window.
//
// Separate from GenerateCert, which refuses a non-positive lifetime, so tests
// can produce the expired and not-yet-valid certificates that the client's
// date check exists to reject.
func generateCert(notBefore, notAfter time.Time) (certPEM, keyPEM []byte, err error) {
	// Ed25519: small keys, fast signatures, and no curve parameters to choose
	// incorrectly. Both ends of this connection are our own binary, so there
	// is no compatibility argument for anything else.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("transport: generate key: %w", err)
	}

	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return nil, nil, fmt.Errorf("transport: generate serial: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "clipd"},
		// The subject and SAN are cosmetic: nothing verifies a hostname,
		// because the pinned key is the identity.
		DNSNames:              []string{"clipd"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, pub, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("transport: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("transport: marshal key: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// EnsureCert generates a keypair at the given paths if one is not already
// there, and reports whether it created anything.
func EnsureCert(certPath, keyPath string, validity time.Duration) (created bool, err error) {
	if fileExists(certPath) && fileExists(keyPath) {
		return false, nil
	}

	if err := writePair(certPath, keyPath, validity); err != nil {
		return false, err
	}
	return true, nil
}

// WriteCert replaces the keypair at the given paths, discarding any existing
// one. Every client pinned to the old key must be reconfigured afterwards.
func WriteCert(certPath, keyPath string, validity time.Duration) error {
	return writePair(certPath, keyPath, validity)
}

// writePair installs a certificate and its key as close to together as POSIX
// allows.
//
// A certificate and the key that signed it are only useful as a pair: install
// one without the other and the daemon fails to start with "private key does
// not match public key", which is a confusing way to learn that a rotation was
// interrupted. Both files are therefore staged completely — generated, written,
// permissioned, closed — before either is renamed into place, so every failure
// mode except one lands with the old pair intact and the daemon still running.
//
// The exception is a failure between the two renames, which cannot be closed
// without a filesystem transaction. It is a far smaller window than writing one
// file's contents, and rotation is a deliberate, foreground command whose
// remedy is to run it again.
func writePair(certPath, keyPath string, validity time.Duration) error {
	certPEM, keyPEM, err := GenerateCert(validity)
	if err != nil {
		return err
	}

	certTmp, err := stageFile(certPath, certPEM, certPerm)
	if err != nil {
		return err
	}
	defer os.Remove(certTmp)

	keyTmp, err := stageFile(keyPath, keyPEM, keyPerm)
	if err != nil {
		return err
	}
	defer os.Remove(keyTmp)

	// Key first: a key without its certificate is recoverable by regenerating,
	// while a certificate whose key was never installed leaves the old private
	// key on disk under a name that no longer describes it.
	if err := os.Rename(keyTmp, keyPath); err != nil {
		return fmt.Errorf("transport: install %s: %w", keyPath, err)
	}
	if err := os.Rename(certTmp, certPath); err != nil {
		return fmt.Errorf("transport: install %s: %w", certPath, err)
	}
	return nil
}

// LoadCertificate reads and parses a PEM certificate, for reporting its
// fingerprint and expiry without starting a server.
func LoadCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("transport: read certificate: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("transport: %s does not contain a PEM certificate", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("transport: parse certificate: %w", err)
	}
	return cert, nil
}

// Fingerprint returns the pinned identity of a certificate: the SHA-256 of its
// Subject Public Key Info.
//
// The public key is hashed rather than the whole certificate so that the
// identity is the key itself. Reissuing a certificate around the same key
// leaves clients working; generating a new key does not, which is correct — a
// new key is a new identity.
func Fingerprint(cert *x509.Certificate) []byte {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return sum[:]
}

// FormatFingerprint renders a digest for display and for config files.
func FormatFingerprint(sum []byte) string {
	return FingerprintPrefix + hex.EncodeToString(sum)
}

// ParseFingerprint accepts the formats a user might reasonably paste: with or
// without the algorithm prefix, with or without colon separators, in any case.
func ParseFingerprint(s string) ([]byte, error) {
	cleaned := strings.TrimSpace(s)
	if cleaned == "" {
		return nil, ErrNoPin
	}
	lower := strings.ToLower(cleaned)
	lower = strings.TrimPrefix(lower, FingerprintPrefix)
	lower = strings.ReplaceAll(lower, ":", "")
	lower = strings.ReplaceAll(lower, " ", "")

	sum, err := hex.DecodeString(lower)
	if err != nil {
		return nil, fmt.Errorf("invalid fingerprint %q: expected hex, optionally prefixed with %s", s, FingerprintPrefix)
	}
	if len(sum) != FingerprintSize {
		return nil, fmt.Errorf("invalid fingerprint %q: got %d bytes, want %d", s, len(sum), FingerprintSize)
	}
	return sum, nil
}

// ServerConfig builds the daemon's TLS configuration.
func ServerConfig(certPath, keyPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("transport: load keypair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		// TLS 1.3 only. Both ends are our own binary, so there is no
		// compatibility argument for anything older, and 1.3 gives forward
		// secrecy on every suite, AEAD-only ciphers, an encrypted handshake,
		// and no downgrade negotiation to reason about.
		MinVersion: tls.VersionTLS13,
	}, nil
}

// ClientConfig builds a configuration that accepts exactly one server key.
func ClientConfig(pin []byte) (*tls.Config, error) {
	if len(pin) != FingerprintSize {
		return nil, ErrNoPin
	}
	pinned := append([]byte(nil), pin...)

	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Alarming at a glance, and correct here: this disables chain and
		// hostname verification, neither of which means anything for a
		// self-signed certificate with no CA and no stable hostname. The
		// check below replaces them and is stricter than the default path —
		// one exact key, rather than anyone signed by a root store.
		//
		// It must never be set without the verifier that follows.
		InsecureSkipVerify: true,
		// VerifyConnection rather than VerifyPeerCertificate: the latter is
		// documented as not being invoked on resumed connections, which would
		// silently bypass the pin. This one runs for every connection.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return ErrNoCertificate
			}
			leaf := cs.PeerCertificates[0]

			// The pin is checked before the dates. A key that does not match
			// is the security-relevant failure and possibly an attack;
			// reporting "expired" for it would point at the wrong problem.
			got := Fingerprint(leaf)
			if subtle.ConstantTimeCompare(got, pinned) != 1 {
				return &PinMismatchError{Want: pinned, Got: got}
			}

			// InsecureSkipVerify above disabled Go's verification path, and
			// that path is what checks the validity period. Without this the
			// NotAfter field would be decorative.
			now := time.Now()
			if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
				return &ValidityError{NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter, Now: now}
			}
			return nil
		},
	}, nil
}

// Ephemeral generates a throwaway keypair and returns a server configuration
// with the pin a client needs to reach it. Intended for tests and for trying
// clipd out; the daemon uses a persistent keypair from disk.
func Ephemeral() (server *tls.Config, pin []byte, err error) {
	certPEM, keyPEM, err := GenerateCert(DefaultValidity)
	if err != nil {
		return nil, nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("transport: build ephemeral keypair: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, nil, fmt.Errorf("transport: parse ephemeral certificate: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}, Fingerprint(leaf), nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// stageFile writes data to a temporary file alongside path and returns its
// name, ready to be renamed into place.
//
// Staging and installing are separate steps so that a caller installing more
// than one file can get every fallible operation — allocation, write, chmod,
// close — out of the way before making any of it visible. The mode is applied
// with Chmod rather than left to OpenFile, so it is not subject to umask, and a
// caller that returns without renaming must remove the temporary file.
func stageFile(path string, data []byte, perm fs.FileMode) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("transport: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, dirPerm); err != nil {
		return "", fmt.Errorf("transport: secure %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("transport: create temporary file: %w", err)
	}
	tmpName := tmp.Name()

	// Any failure past this point leaves a temporary file behind, so each one
	// removes it: the caller has no name to clean up until this returns.
	fail := func(format string, err error) (string, error) {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf(format, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		return fail("transport: set permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail("transport: write: %w", err)
	}
	// Sync before the rename: without it the directory entry can reach disk
	// ahead of the contents, and a machine that loses power mid-rotation comes
	// back with a correctly named, empty private key.
	if err := tmp.Sync(); err != nil {
		return fail("transport: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("transport: close: %w", err)
	}
	return tmpName, nil
}
