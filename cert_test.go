package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

// Expired certificates are accepted only under expiredOK, and only if chain and hostname still verify.
func TestVerifyCert(t *testing.T) {
	now := time.Now()
	newCA := func() (*x509.Certificate, *ecdsa.PrivateKey) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true,
			BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-2 * 365 * 24 * time.Hour), NotAfter: now.Add(365 * 24 * time.Hour)}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		c, _ := x509.ParseCertificate(der)
		return c, key
	}
	ca, key := newCA()
	untrusted, untrustedKey := newCA()
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	leaf := func(name string, notAfter time.Time, parent *x509.Certificate, signer *ecdsa.PrivateKey) *x509.Certificate {
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{name}, NotBefore: now.Add(-365 * 24 * time.Hour),
			NotAfter: notAfter, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &signer.PublicKey, signer)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		return c
	}
	expired, valid := now.Add(-24*time.Hour), now.Add(24*time.Hour)
	for _, c := range []struct {
		name, server string
		cert         *x509.Certificate
		ok           bool
	}{
		{"valid", "a.example.com", leaf("a.example.com", valid, ca, key), true},
		{"expired, other domain", "a.example.com", leaf("a.example.com", expired, ca, key), false},
		{"expired cdn", "x1.cloudatacdn.com", leaf("*.cloudatacdn.com", expired, ca, key), true},
		{"expired cdn, wrong name", "x1.cloudatacdn.com", leaf("y.example.com", expired, ca, key), false},
		{"expired cdn, untrusted CA", "x1.cloudatacdn.com", leaf("*.cloudatacdn.com", expired, untrusted, untrustedKey), false},
	} {
		err := verifyCert(tls.ConnectionState{ServerName: c.server, PeerCertificates: []*x509.Certificate{c.cert}}, roots)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}
