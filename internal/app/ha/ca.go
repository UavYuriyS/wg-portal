package ha

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// The cluster CA signs the TLS certificates Galera members use to authenticate each other.

func NewCa(clusterName string) (certPem, keyPem string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	tpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: clusterName + " cluster CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	return encodePem(der, key)
}

// SignMemberCert issues a certificate usable as TLS server and client for one cluster member.
func SignMemberCert(caCertPem, caKeyPem, memberId, memberIp string) (certPem, keyPem string, err error) {
	caCert, caKey, err := parseCa(caCertPem, caKeyPem)
	if err != nil {
		return "", "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	tpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: memberId},
		DNSNames:     []string{memberId},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	if ip := net.ParseIP(memberIp); ip != nil {
		tpl.IPAddresses = []net.IP{ip}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return "", "", err
	}
	return encodePem(der, key)
}

func parseCa(certPem, keyPem string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, _ := pem.Decode([]byte(certPem))
	kb, _ := pem.Decode([]byte(keyPem))
	if cb == nil || kb == nil {
		return nil, nil, fmt.Errorf("invalid CA pem")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func encodePem(der []byte, key *ecdsa.PrivateKey) (string, string, error) {
	keyDer, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer})), nil
}

func randomSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}
