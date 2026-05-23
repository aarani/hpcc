// Package bootstrap holds first-run helpers shared by `hpcc init
// scheduler` and `hpcc init worker`: generating a worker-token suitable
// for the scheduler<->worker shared secret, and minting a self-signed
// TLS leaf for components whose peer pins by fingerprint (the worker).
package bootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// GenerateToken returns a 32-byte cryptographically random URL-safe
// token. That's 256 bits of entropy, comfortably above the 16-character
// floor scheduler.Validate / worker.Validate enforce.
func GenerateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// SelfSignedOptions controls the leaf cert minted by
// GenerateSelfSignedTLS. CommonName is informational; the leaf is bound
// to peers via the SHA-256 fingerprint the scheduler records at worker
// registration, not via name validation. Hosts populates SANs so that
// operators who do choose to verify by name (e.g. an operator running a
// worker behind a stable DNS name) have something to verify against.
type SelfSignedOptions struct {
	CommonName string
	Hosts      []string      // DNS names / IPs to put in SANs
	NotAfter   time.Duration // validity duration; <=0 → 10 years
}

// GenerateSelfSignedTLS mints an ECDSA P-256 self-signed certificate +
// private key and returns them PEM-encoded. The output is what
// secret.LoadTLSCertificate consumes when [tls] cert_file/key_file
// point at on-disk files.
func GenerateSelfSignedTLS(opts SelfSignedOptions) (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	validity := opts.NotAfter
	if validity <= 0 {
		validity = 10 * 365 * 24 * time.Hour
	}

	cn := opts.CommonName
	if cn == "" {
		cn = "hpcc"
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		BasicConstraintsValid: true,
	}
	for _, h := range opts.Hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
