// Package identity selects the gRPC credential source for the broker:
// either a SPIFFE Workload-API X509Source (in-cluster default) or static
// PEM files (compose / dev without SPIRE).
package identity

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// Mode describes which credential path was selected.
type Mode string

const (
	ModeFile   Mode = "file"
	ModeSocket Mode = "socket"

	// southTrustDomain is the single source of truth for the trust domain
	// enforced on south (sandbox→broker) mTLS peers.
	southTrustDomain = "aikonos.com"
)

// SouthAuthorizer returns the AuthorizeMemberOf gate for the south gRPC server.
// Both the file-mode and socket-mode paths in main.go use this — the trust
// domain literal lives here and nowhere else.
func SouthAuthorizer() tlsconfig.Authorizer {
	return tlsconfig.AuthorizeMemberOf(spiffeid.RequireTrustDomainFromString(southTrustDomain))
}

// Config carries the inputs for SelectCredSource.
type Config struct {
	CertFile     string // AIKONOS_BROKER_TLS_CERT_FILE
	KeyFile      string // AIKONOS_BROKER_TLS_KEY_FILE
	CAFile       string // AIKONOS_BROKER_TLS_CA_FILE
	SpiffeSocket string // passed through to workloadapi.NewX509Source in main.go; not used by SelectCredSource itself
}

// Source holds the two interfaces MTLSServerCredentials needs plus the
// south authorizer so callers can reconstruct the exact same gate.
type Source struct {
	SVIDSource   x509svid.Source
	BundleSource x509bundle.Source
	// southTD is the trust domain used to build the AuthorizeMemberOf gate.
	southTD spiffeid.TrustDomain
}

// SouthAuthorizer returns AuthorizeMemberOf(aikonos.com) — the same gate
// the south gRPC server uses. Exposed so tests can verify the gate applies.
func (s *Source) SouthAuthorizer() tlsconfig.Authorizer {
	return tlsconfig.AuthorizeMemberOf(s.southTD)
}

// staticSVIDSource wraps a loaded *x509svid.SVID to satisfy x509svid.Source.
type staticSVIDSource struct{ svid *x509svid.SVID }

func (s *staticSVIDSource) GetX509SVID() (*x509svid.SVID, error) { return s.svid, nil }

// SelectCredSource decides which credential source to use and returns it.
//
// Selection rule (deterministic, fail-closed):
//   - All three file keys set → load static PEM sources.
//   - Any file key empty → socket/workload-api mode (caller still connects to the
//     socket; we return a nil Source so the caller knows to use workloadapi directly).
//
// The socket fallback (prefer files when socket fails) is handled in main.go:
// when files are valid, main uses file mode regardless; socket is tried only
// when no file keys are set.
func SelectCredSource(cfg Config) (*Source, Mode, error) {
	filesSet := cfg.CertFile != "" && cfg.KeyFile != "" && cfg.CAFile != ""
	if !filesSet {
		// Socket/workload-api path — caller builds the X509Source itself.
		return nil, ModeSocket, nil
	}

	svid, err := x509svid.Load(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, "", fmt.Errorf("identity: load SVID from PEM: %w", err)
	}

	bundle, err := loadBundle(cfg.CAFile)
	if err != nil {
		return nil, "", fmt.Errorf("identity: load CA bundle: %w", err)
	}

	td := spiffeid.RequireTrustDomainFromString(southTrustDomain)
	src := &Source{
		SVIDSource:   &staticSVIDSource{svid: svid},
		BundleSource: bundle,
		southTD:      td,
	}
	return src, ModeFile, nil
}

// loadBundle reads a PEM file of one or more CA certificates and returns an
// x509bundle.Bundle for the aikonos.com trust domain.
func loadBundle(caFile string) (*x509bundle.Bundle, error) {
	raw, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}

	var certs []*x509.Certificate
	rest := raw
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse CA cert: %w", err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no CERTIFICATE blocks found in %s", caFile)
	}

	td := spiffeid.RequireTrustDomainFromString(southTrustDomain)
	return x509bundle.FromX509Authorities(td, certs), nil
}
