package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGenerateCert(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM, err := GenerateCert(DefaultValidity)
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("the generated PEM is not a usable keypair: %v", err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	// Self-signed: issuer and subject are the same, and it verifies against
	// itself.
	if cert.Issuer.String() != cert.Subject.String() {
		t.Errorf("issuer %q != subject %q; the certificate is not self-signed",
			cert.Issuer, cert.Subject)
	}
	// CheckSignatureFrom is not usable here: it enforces CA constraints, and
	// this is deliberately a non-CA leaf. Verifying the signature directly is
	// the property that matters — the key in the certificate signed it.
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		t.Errorf("certificate is not signed by its own key: %v", err)
	}

	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		t.Error("a freshly generated certificate is not currently valid")
	}
	// Backdated so a clock a few minutes out on either machine does not
	// produce a baffling "not valid until" failure on a fresh install.
	if !cert.NotBefore.Before(now) {
		t.Error("NotBefore was not backdated")
	}
	if remaining := time.Until(cert.NotAfter); remaining < 9*365*24*time.Hour {
		t.Errorf("validity is %s, want roughly ten years", remaining)
	}
	if cert.IsCA {
		t.Error("the certificate claims to be a CA")
	}
}

func TestGenerateCertRejectsBadValidity(t *testing.T) {
	t.Parallel()

	for _, d := range []time.Duration{0, -time.Hour} {
		if _, _, err := GenerateCert(d); err == nil {
			t.Errorf("GenerateCert(%s) accepted a non-positive validity", d)
		}
	}
}

func TestFingerprintIdentifiesTheKey(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM, err := GenerateCert(DefaultValidity)
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	first := parsePEM(t, certPEM, keyPEM)

	// Stable across reloads of the same material.
	again := parsePEM(t, certPEM, keyPEM)
	if FormatFingerprint(Fingerprint(first)) != FormatFingerprint(Fingerprint(again)) {
		t.Error("fingerprint is not stable across reloads")
	}

	// Different for a different key, which is what makes it an identity.
	otherCert, otherKey, err := GenerateCert(DefaultValidity)
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	other := parsePEM(t, otherCert, otherKey)
	if FormatFingerprint(Fingerprint(first)) == FormatFingerprint(Fingerprint(other)) {
		t.Error("two independently generated keys share a fingerprint")
	}
}

func TestParseFingerprint(t *testing.T) {
	t.Parallel()

	canonical := "sha256:" + strings.Repeat("ab", FingerprintSize)
	want, err := ParseFingerprint(canonical)
	if err != nil {
		t.Fatalf("ParseFingerprint: %v", err)
	}

	// Everything a user might plausibly paste normalises to the same bytes.
	equivalents := []string{
		canonical,
		strings.Repeat("ab", FingerprintSize), // no prefix
		strings.ToUpper(strings.Repeat("ab", FingerprintSize)),  // upper case
		"SHA256:" + strings.Repeat("AB", FingerprintSize),       // upper prefix
		strings.Join(splitPairs(strings.Repeat("ab", 32)), ":"), // colon separated
		"  " + canonical + "  ",                                 // padded
	}
	for _, form := range equivalents {
		got, err := ParseFingerprint(form)
		if err != nil {
			t.Errorf("ParseFingerprint(%q): %v", form, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("ParseFingerprint(%q) produced different bytes", form)
		}
	}

	for _, bad := range []string{
		"",
		"sha256:",
		"not-hex-at-all",
		strings.Repeat("ab", FingerprintSize-1), // too short
		strings.Repeat("ab", FingerprintSize+1), // too long
		"sha512:" + strings.Repeat("ab", 64),    // wrong algorithm, wrong length
	} {
		if _, err := ParseFingerprint(bad); err == nil {
			t.Errorf("ParseFingerprint(%q) accepted an invalid fingerprint", bad)
		}
	}
}

func TestClientConfigRequiresAPin(t *testing.T) {
	t.Parallel()

	for _, pin := range [][]byte{nil, {}, make([]byte, FingerprintSize-1)} {
		if _, err := ClientConfig(pin); !errors.Is(err, ErrNoPin) {
			t.Errorf("ClientConfig(%d bytes) = %v, want ErrNoPin", len(pin), err)
		}
	}
}

func TestEnsureCert(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls", "cert.pem")
	keyPath := filepath.Join(dir, "tls", "key.pem")

	created, err := EnsureCert(certPath, keyPath, DefaultValidity)
	if err != nil {
		t.Fatalf("EnsureCert: %v", err)
	}
	if !created {
		t.Error("EnsureCert reported no work on an empty directory")
	}

	first, err := LoadCertificate(certPath)
	if err != nil {
		t.Fatalf("LoadCertificate: %v", err)
	}

	// The private key must not be readable by anyone else. Windows has no
	// Unix permission bits, so the check only means something elsewhere.
	if runtime.GOOS != "windows" {
		keyInfo, err := os.Stat(keyPath)
		if err != nil {
			t.Fatalf("stat key: %v", err)
		}
		if perm := keyInfo.Mode().Perm(); perm != keyPerm {
			t.Errorf("key mode = %04o, want %04o", perm, keyPerm)
		}
		dirInfo, err := os.Stat(filepath.Dir(keyPath))
		if err != nil {
			t.Fatalf("stat directory: %v", err)
		}
		if perm := dirInfo.Mode().Perm(); perm != dirPerm {
			t.Errorf("tls directory mode = %04o, want %04o", perm, dirPerm)
		}
	}

	// Idempotent: a second call must not silently re-key and break clients.
	created, err = EnsureCert(certPath, keyPath, DefaultValidity)
	if err != nil {
		t.Fatalf("second EnsureCert: %v", err)
	}
	if created {
		t.Error("EnsureCert regenerated an existing keypair")
	}
	second, err := LoadCertificate(certPath)
	if err != nil {
		t.Fatalf("LoadCertificate: %v", err)
	}
	if string(Fingerprint(first)) != string(Fingerprint(second)) {
		t.Error("EnsureCert changed the key on an existing installation")
	}
}

func TestWriteCertRotates(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	if _, err := EnsureCert(certPath, keyPath, DefaultValidity); err != nil {
		t.Fatalf("EnsureCert: %v", err)
	}
	before, err := LoadCertificate(certPath)
	if err != nil {
		t.Fatalf("LoadCertificate: %v", err)
	}

	if err := WriteCert(certPath, keyPath, DefaultValidity); err != nil {
		t.Fatalf("WriteCert: %v", err)
	}
	after, err := LoadCertificate(certPath)
	if err != nil {
		t.Fatalf("LoadCertificate: %v", err)
	}
	if string(Fingerprint(before)) == string(Fingerprint(after)) {
		t.Error("WriteCert did not produce a new key")
	}
}

// handshake runs a real TLS exchange between a pinned client and a server
// holding the given certificate, returning the client's handshake error.
func handshake(t *testing.T, serverConfig *tls.Config, pin []byte) error {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		tlsConn := tls.Server(conn, serverConfig)
		_ = tlsConn.HandshakeContext(context.Background())
	}()

	clientConfig, err := ClientConfig(pin)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	rawConn, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer rawConn.Close()
	_ = rawConn.SetDeadline(time.Now().Add(10 * time.Second))

	conn := tls.Client(rawConn, clientConfig)
	return conn.HandshakeContext(context.Background())
}

func TestHandshakeAcceptsThePinnedKey(t *testing.T) {
	t.Parallel()

	serverConfig, pin, err := Ephemeral()
	if err != nil {
		t.Fatalf("Ephemeral: %v", err)
	}
	if err := handshake(t, serverConfig, pin); err != nil {
		t.Errorf("handshake with the correct pin failed: %v", err)
	}
}

func TestHandshakeRejectsAnotherKey(t *testing.T) {
	t.Parallel()

	serverConfig, _, err := Ephemeral()
	if err != nil {
		t.Fatalf("Ephemeral: %v", err)
	}
	_, otherPin, err := Ephemeral()
	if err != nil {
		t.Fatalf("Ephemeral: %v", err)
	}

	err = handshake(t, serverConfig, otherPin)
	if err == nil {
		t.Fatal("handshake succeeded against a server with a different key")
	}
	var mismatch *PinMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error = %v (%T), want PinMismatchError", err, err)
	}
	// Both fingerprints belong in the message: the user needs to see whether
	// this is their own rotation or something else.
	if !strings.Contains(mismatch.Error(), FormatFingerprint(mismatch.Want)) ||
		!strings.Contains(mismatch.Error(), FormatFingerprint(mismatch.Got)) {
		t.Errorf("message does not show both fingerprints: %s", mismatch)
	}
}

// TestHandshakeRejectsAnExpiredCertificate is the regression test for the
// subtlest part of this design.
//
// InsecureSkipVerify switches off Go's verification path, and that path is
// what checks NotBefore/NotAfter. Without the explicit check in
// VerifyConnection, the validity period would be decorative and an expired
// certificate would be accepted indefinitely. If someone removes that check,
// this fails.
func TestHandshakeRejectsAnExpiredCertificate(t *testing.T) {
	t.Parallel()

	serverConfig, pin := expiringConfig(t, -48*time.Hour)

	err := handshake(t, serverConfig, pin)
	if err == nil {
		t.Fatal("handshake accepted an expired certificate")
	}
	var validity *ValidityError
	if !errors.As(err, &validity) {
		t.Fatalf("error = %v (%T), want ValidityError", err, err)
	}
	if !strings.Contains(validity.Error(), "expired") {
		t.Errorf("message = %q, want it to say the certificate expired", validity)
	}
}

// TestHandshakeAcceptsACertificateAboutToExpire confirms the check is the
// date itself and not a freshness policy that would reject a valid key.
func TestHandshakeAcceptsACertificateAboutToExpire(t *testing.T) {
	t.Parallel()

	serverConfig, pin := expiringConfig(t, time.Hour)
	if err := handshake(t, serverConfig, pin); err != nil {
		t.Errorf("handshake rejected a certificate that is still valid: %v", err)
	}
}

// TestPinMismatchOutranksExpiry: when both are wrong, the reported problem is
// the key. A wrong key is potentially an attack; reporting "expired" would
// send the user off fixing the wrong thing.
func TestPinMismatchOutranksExpiry(t *testing.T) {
	t.Parallel()

	serverConfig, _ := expiringConfig(t, -48*time.Hour)
	_, otherPin, err := Ephemeral()
	if err != nil {
		t.Fatalf("Ephemeral: %v", err)
	}

	err = handshake(t, serverConfig, otherPin)
	if err == nil {
		t.Fatal("handshake succeeded with both a wrong pin and an expired certificate")
	}
	var mismatch *PinMismatchError
	if !errors.As(err, &mismatch) {
		t.Errorf("error = %v (%T), want the pin mismatch to be reported first", err, err)
	}
}

func TestServerConfigRequiresRealFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if _, err := ServerConfig(filepath.Join(dir, "absent.pem"), filepath.Join(dir, "absent.key")); err == nil {
		t.Error("ServerConfig accepted missing files")
	}
}

func TestLoadCertificateRejectsNonPEM(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(path, []byte("this is not a certificate"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadCertificate(path); err == nil {
		t.Error("LoadCertificate accepted a file that is not PEM")
	}
}

// TestTLSVersionIsPinnedTo13 guards the floor: anything older loses the
// guaranteed forward secrecy that makes captured traffic useless later.
func TestTLSVersionIsPinnedTo13(t *testing.T) {
	t.Parallel()

	serverConfig, pin, err := Ephemeral()
	if err != nil {
		t.Fatalf("Ephemeral: %v", err)
	}
	if serverConfig.MinVersion != tls.VersionTLS13 {
		t.Errorf("server MinVersion = 0x%04x, want TLS 1.3", serverConfig.MinVersion)
	}
	clientConfig, err := ClientConfig(pin)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	if clientConfig.MinVersion != tls.VersionTLS13 {
		t.Errorf("client MinVersion = 0x%04x, want TLS 1.3", clientConfig.MinVersion)
	}
	// InsecureSkipVerify without a verifier would accept any server at all.
	if clientConfig.InsecureSkipVerify && clientConfig.VerifyConnection == nil {
		t.Fatal("InsecureSkipVerify is set with no VerifyConnection to replace it")
	}
}

// expiringConfig builds a server config whose certificate expires at
// now+offset; a negative offset produces an already-expired certificate.
func expiringConfig(t *testing.T, offset time.Duration) (*tls.Config, []byte) {
	t.Helper()

	now := time.Now()
	certPEM, keyPEM, err := generateCert(now.Add(-365*24*time.Hour), now.Add(offset))
	if err != nil {
		t.Fatalf("generateCert: %v", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS13,
	}, Fingerprint(leaf)
}

func parsePEM(t *testing.T, certPEM, keyPEM []byte) *x509.Certificate {
	t.Helper()
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}

func splitPairs(s string) []string {
	var out []string
	for i := 0; i+2 <= len(s); i += 2 {
		out = append(out, s[i:i+2])
	}
	return out
}

// TestRotationLeavesAMatchingPairAndNoDebris covers what makes a keypair
// different from two files: installing a certificate without the key that
// signed it leaves a daemon that refuses to start, so both must arrive
// together and neither may leave a staging file behind.
func TestRotationLeavesAMatchingPairAndNoDebris(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls", "cert.pem")
	keyPath := filepath.Join(dir, "tls", "key.pem")

	for i, install := range []func() error{
		func() error { _, err := EnsureCert(certPath, keyPath, DefaultValidity); return err },
		func() error { return WriteCert(certPath, keyPath, DefaultValidity) },
		func() error { return WriteCert(certPath, keyPath, DefaultValidity) },
	} {
		if err := install(); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}

		// The pair must load together: this is the check the daemon itself
		// performs at startup.
		if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
			t.Fatalf("install %d produced a mismatched pair: %v", i, err)
		}

		entries, err := os.ReadDir(filepath.Dir(certPath))
		if err != nil {
			t.Fatalf("read tls dir: %v", err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp-") {
				t.Errorf("install %d left a staging file behind: %s", i, e.Name())
			}
		}
		if len(entries) != 2 {
			t.Errorf("install %d left %d files in the tls directory, want 2", i, len(entries))
		}
	}
}

func TestRotatedKeyKeepsRestrictivePermissions(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls", "cert.pem")
	keyPath := filepath.Join(dir, "tls", "key.pem")

	if _, err := EnsureCert(certPath, keyPath, DefaultValidity); err != nil {
		t.Fatalf("EnsureCert: %v", err)
	}
	if err := WriteCert(certPath, keyPath, DefaultValidity); err != nil {
		t.Fatalf("WriteCert: %v", err)
	}

	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	// Rotation must not be a way to widen the key's mode: it is written with
	// Chmod precisely so that umask cannot loosen it.
	if got := info.Mode().Perm(); got != keyPerm {
		t.Errorf("rotated key mode = %04o, want %04o", got, keyPerm)
	}
}
