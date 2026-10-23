// Package tunnel implements the TLS transport and session lifecycle for both
// ends of a MyTun tunnel.
package tunnel

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// CertPair holds a PEM encoded certificate and key.
type CertPair struct {
	CertPEM []byte
	KeyPEM  []byte
	Cert    tls.Certificate
}

// LoadOrCreateCert returns a cached self-signed certificate for the given
// common name, generating one on first use. Certificates are valid for 10
// years and cover the CN plus wildcard DNS names, mirroring the behaviour of
// Bi-Tunnel's node-forge generator.
func LoadOrCreateCert(dir, commonName string) (*CertPair, error) {
	if commonName == "" {
		commonName = "mail.qq.com"
	}
	if dir != "" {
		certPath := filepath.Join(dir, "tls-cert.pem")
		keyPath := filepath.Join(dir, "tls-key.pem")
		if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
			certPEM, _ := os.ReadFile(certPath)
			keyPEM, _ := os.ReadFile(keyPath)
			return &CertPair{CertPEM: certPEM, KeyPEM: keyPEM, Cert: c}, nil
		}
	}
	pair, err := generateCert(commonName)
	if err != nil {
		return nil, err
	}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "tls-cert.pem"), pair.CertPEM, 0o644)
			_ = os.WriteFile(filepath.Join(dir, "tls-key.pem"), pair.KeyPEM, 0o600)
		}
	}
	return pair, nil
}

func generateCert(cn string) (*CertPair, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("tunnel: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"MyTun"}},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{cn, "*." + cn, "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("tunnel: create cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &CertPair{CertPEM: certPEM, KeyPEM: keyPEM, Cert: cert}, nil
}
