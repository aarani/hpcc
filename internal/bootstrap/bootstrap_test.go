package bootstrap

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
)

func TestGenerateToken_LengthAndEntropy(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	// Worker/scheduler validation requires >= 16 chars; 32 random
	// bytes base64'd should comfortably exceed that.
	if len(a) < 16 {
		t.Fatalf("token shorter than 16 chars: %q", a)
	}
	if a == b {
		t.Fatal("two GenerateToken() calls returned identical tokens — randomness broken")
	}
}

func TestGenerateSelfSignedTLS_LoadsAsKeyPair(t *testing.T) {
	certPEM, keyPEM, err := GenerateSelfSignedTLS(SelfSignedOptions{
		CommonName: "worker.test",
		Hosts:      []string{"worker.test", "127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair rejected generated material: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "worker.test" {
		t.Errorf("CommonName = %q, want %q", leaf.Subject.CommonName, "worker.test")
	}
	// Hosts split between DNSNames and IPAddresses by net.ParseIP.
	gotDNS := false
	for _, n := range leaf.DNSNames {
		if n == "worker.test" {
			gotDNS = true
		}
	}
	if !gotDNS {
		t.Errorf("DNSNames = %v, want it to include %q", leaf.DNSNames, "worker.test")
	}
	gotIP := false
	for _, ip := range leaf.IPAddresses {
		if ip.String() == "127.0.0.1" {
			gotIP = true
		}
	}
	if !gotIP {
		t.Errorf("IPAddresses = %v, want it to include 127.0.0.1", leaf.IPAddresses)
	}
}
