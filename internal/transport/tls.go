package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// ALPN is the application protocol negotiated on every KV stream.
const ALPN = "gospark-kv1"

// CertFingerprint returns the hex SHA-256 of a DER certificate. This is the
// value a peer pins: it is stable, cheap to compare, and independent of any CA.
func CertFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// ephemeralCertificate builds one short-lived self-signed certificate for the
// data plane and returns it with the fingerprint a peer must pin. The usage is
// explicit: a listener certificate is ServerAuth, a client certificate is
// ClientAuth. A deployed node replaces this with a certificate bound to its
// overlay identity; the pin mechanism is what stays the same.
func ephemeralCertificate(usage x509.ExtKeyUsage, commonName string) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("transport: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("transport: serial: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{usage},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("transport: create cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("transport: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("transport: keypair: %w", err)
	}
	return cert, CertFingerprint(der), nil
}

// selfSignedTLS builds the listener's TLS config and its pin.
func selfSignedTLS() (*tls.Config, string, error) {
	cert, fingerprint, err := ephemeralCertificate(x509.ExtKeyUsageServerAuth, "gospark-data-plane")
	if err != nil {
		return nil, "", err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{ALPN},
		MinVersion:   tls.VersionTLS13,
	}, fingerprint, nil
}

// NewEphemeralClientCertificate builds a client certificate for mutual TLS.
// The caller gives its fingerprint to the server's ClientFingerprint pin.
func NewEphemeralClientCertificate() (tls.Certificate, string, error) {
	return ephemeralCertificate(x509.ExtKeyUsageClientAuth, "gospark-data-plane-client")
}
