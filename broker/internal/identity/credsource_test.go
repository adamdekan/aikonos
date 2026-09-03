package identity_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/adamdekan/aikonos/broker/internal/identity"
)

// genCert mints a CA + leaf ECDSA cert chain.
// The leaf carries the given URI-SAN and DNS-SAN and is NOT a CA cert
// (go-spiffe x509svid.Load rejects leaf certs with IsCA=true).
// Returns (certPath, keyPath, caPath) written under dir.
func genCert(t *testing.T, dir, spiffeURI, dnsName string) (certPath, keyPath, caPath string) {
	t.Helper()

	// CA key + self-signed cert
	caPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca keygen: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caPriv.PublicKey, caPriv)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}

	// Leaf key + cert signed by CA (not a CA cert itself)
	leafPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf keygen: %v", err)
	}
	spiffeURL, err := url.Parse(spiffeURI)
	if err != nil {
		t.Fatalf("parse URI: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		URIs:         []*url.URL{spiffeURL},
		DNSNames:     []string{dnsName},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafPriv.PublicKey, caPriv)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	caPath = filepath.Join(dir, "ca.pem")

	writePEM(t, certPath, "CERTIFICATE", leafDER)
	// go-spiffe x509svid.Load expects PKCS8 ("PRIVATE KEY"), not SEC1 ("EC PRIVATE KEY").
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafPriv)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	writePEM(t, keyPath, "PRIVATE KEY", leafKeyDER)
	writePEM(t, caPath, "CERTIFICATE", caDER)
	return certPath, keyPath, caPath
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		t.Fatalf("pem encode %s: %v", path, err)
	}
}

// TestFileMode_ValidPEMs: file paths + valid PEMs → file mode selected, sources usable.
func TestFileMode_ValidPEMs(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, caPath := genCert(t, dir, "spiffe://aikonos.com/broker", "broker")

	src, mode, err := identity.SelectCredSource(identity.Config{
		CertFile:    certPath,
		KeyFile:     keyPath,
		CAFile:      caPath,
		SpiffeSocket: "",
	})
	if err != nil {
		t.Fatalf("SelectCredSource: %v", err)
	}
	if mode != identity.ModeFile {
		t.Fatalf("expected mode %q, got %q", identity.ModeFile, mode)
	}
	svid, err := src.SVIDSource.GetX509SVID()
	if err != nil {
		t.Fatalf("GetX509SVID: %v", err)
	}
	if svid == nil {
		t.Fatal("nil SVID")
	}

	td := spiffeid.RequireTrustDomainFromString("aikonos.com")
	bundle, err := src.BundleSource.GetX509BundleForTrustDomain(td)
	if err != nil {
		t.Fatalf("GetX509BundleForTrustDomain: %v", err)
	}
	if bundle == nil {
		t.Fatal("nil bundle")
	}
}

// TestSocketMode_EmptyFiles: no file paths → socket mode selected (no live socket needed).
func TestSocketMode_EmptyFiles(t *testing.T) {
	_, mode, err := identity.SelectCredSource(identity.Config{
		CertFile:    "",
		KeyFile:     "",
		CAFile:      "",
		SpiffeSocket: "unix:///run/spire/agent/api.sock",
	})
	if err != nil {
		t.Fatalf("SelectCredSource (socket mode, no live socket): %v", err)
	}
	if mode != identity.ModeSocket {
		t.Fatalf("expected mode %q, got %q", identity.ModeSocket, mode)
	}
}

// TestFileMode_MalformedPEM: malformed cert → error, no panic.
func TestFileMode_MalformedPEM(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	caPath := filepath.Join(dir, "ca.pem")

	if err := os.WriteFile(certPath, []byte("not a pem"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("not a pem"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, []byte("not a pem"), 0600); err != nil {
		t.Fatal(err)
	}

	_, _, err := identity.SelectCredSource(identity.Config{
		CertFile:    certPath,
		KeyFile:     keyPath,
		CAFile:      caPath,
		SpiffeSocket: "",
	})
	if err == nil {
		t.Fatal("expected error for malformed PEM, got nil")
	}
}

// TestFileMode_MissingFile: cert path does not exist → error, no panic.
func TestFileMode_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, _, err := identity.SelectCredSource(identity.Config{
		CertFile:    filepath.Join(dir, "absent.pem"),
		KeyFile:     filepath.Join(dir, "absent-key.pem"),
		CAFile:      filepath.Join(dir, "absent-ca.pem"),
		SpiffeSocket: "",
	})
	if err == nil {
		t.Fatal("expected error for missing PEM, got nil")
	}
}

// TestFileMode_WrongTrustDomain: the file-path sources, when used with
// AuthorizeMemberOf(aikonos.com), must reject a peer whose URI-SAN is in a
// different trust domain (spiffe://evil.test/x).
//
// Strategy: build the authorizer used on the south server and directly verify
// that it rejects a spiffe ID from the wrong domain, AND confirm that
// SelectCredSource constructs creds using exactly that authorizer shape
// (AuthorizeMemberOf wrapping aikonos.com).  A full TLS handshake is impractical
// without a live listener; the authorizer-level assertion is the meaningful gate.
func TestFileMode_WrongTrustDomain_Rejected(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, caPath := genCert(t, dir, "spiffe://aikonos.com/broker", "broker")

	src, mode, err := identity.SelectCredSource(identity.Config{
		CertFile:    certPath,
		KeyFile:     keyPath,
		CAFile:      caPath,
		SpiffeSocket: "",
	})
	if err != nil {
		t.Fatalf("SelectCredSource: %v", err)
	}
	if mode != identity.ModeFile {
		t.Fatalf("expected file mode, got %q", mode)
	}
	// Use the exact authorizer wired in main.go — the single source of truth.
	authorizer := identity.SouthAuthorizer()

	wrongID := spiffeid.RequireFromString("spiffe://evil.test/x")
	goodID := spiffeid.RequireFromString("spiffe://aikonos.com/agent-gateway")

	if authErr := authorizer(wrongID, nil); authErr == nil {
		t.Fatal("SouthAuthorizer must reject spiffe://evil.test/x")
	}
	if authErr := authorizer(goodID, nil); authErr != nil {
		t.Fatalf("SouthAuthorizer must accept spiffe://aikonos.com/agent-gateway, got: %v", authErr)
	}

	// Source.SouthAuthorizer() delegates to the same gate.
	srcAuthorizer := src.SouthAuthorizer()
	if authErr := srcAuthorizer(wrongID, nil); authErr == nil {
		t.Fatal("src.SouthAuthorizer must reject spiffe://evil.test/x")
	}
	if authErr := srcAuthorizer(goodID, nil); authErr != nil {
		t.Fatalf("src.SouthAuthorizer must accept spiffe://aikonos.com/agent-gateway, got: %v", authErr)
	}
}
