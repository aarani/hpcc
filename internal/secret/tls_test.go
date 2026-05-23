package secret

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeSelfSigned creates a fresh self-signed cert+key for a test. PEM
// bytes are returned in (cert, key) order; X509KeyPair must accept the
// pair.
func makeSelfSigned(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "hpcc-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return
}

func TestLoadTLSCertificate_Files(t *testing.T) {
	certPEM, keyPEM := makeSelfSigned(t)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	cert, gotPEM, err := LoadTLSCertificate(context.Background(), &Resolver{}, certPath, "", keyPath, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("LoadTLSCertificate returned empty Certificate")
	}
	if string(gotPEM) != string(certPEM) {
		t.Fatal("returned cert PEM does not match input")
	}
}

func TestLoadTLSCertificate_Refs(t *testing.T) {
	certPEM, keyPEM := makeSelfSigned(t)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	cert, gotPEM, err := LoadTLSCertificate(context.Background(), &Resolver{}, "", "file:"+certPath, "", "file:"+keyPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("LoadTLSCertificate returned empty Certificate")
	}
	if string(gotPEM) != string(certPEM) {
		t.Fatal("returned cert PEM does not match input")
	}
}

func TestLoadTLSCertificate_BothSetRejected(t *testing.T) {
	_, _, err := LoadTLSCertificate(context.Background(), &Resolver{}, "/a", "file:/b", "/c", "")
	if err == nil {
		t.Fatal("expected error when both cert_file and cert_ref are set")
	}
}

func TestLoadTLSCertificate_NoneSetRejected(t *testing.T) {
	_, _, err := LoadTLSCertificate(context.Background(), &Resolver{}, "", "", "/c", "")
	if err == nil {
		t.Fatal("expected error when neither cert_file nor cert_ref is set")
	}
}

func TestLoadTLSCertificate_SMRef(t *testing.T) {
	certPEM, keyPEM := makeSelfSigned(t)
	r := &Resolver{
		awsSM: func(ctx context.Context, region, secretID string) (string, []byte, error) {
			switch secretID {
			case "hpcc/tls/cert":
				return string(certPEM), nil, nil
			case "hpcc/tls/key":
				return string(keyPEM), nil, nil
			}
			t.Errorf("unexpected secret id %q", secretID)
			return "", nil, nil
		},
	}
	cert, _, err := LoadTLSCertificate(context.Background(), r, "", "aws-sm://hpcc/tls/cert", "", "aws-sm://hpcc/tls/key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("LoadTLSCertificate returned empty Certificate")
	}
}
