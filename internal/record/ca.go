package record

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CA file names inside the CA directory.
const (
	CACertFile = "lt-ca.pem"
	CAKeyFile  = "lt-ca-key.pem"
)

// CA is a local certificate authority used to intercept HTTPS (--mitm).
// Leaf certificates are made per host on demand and cached in memory.
type CA struct {
	Cert     *x509.Certificate
	CertPath string
	key      *ecdsa.PrivateKey
	certDER  []byte
	leafKey  *ecdsa.PrivateKey

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// LoadOrCreateCA reads the CA from dir, creating it (and dir) on first use.
// created reports whether a new CA was generated.
func LoadOrCreateCA(dir string) (ca *CA, created bool, err error) {
	certPath, keyPath := filepath.Join(dir, CACertFile), filepath.Join(dir, CAKeyFile)
	certPEM, err1 := os.ReadFile(certPath)
	keyPEM, err2 := os.ReadFile(keyPath)
	switch {
	case err1 == nil && err2 == nil:
		ca, err = parseCA(certPEM, keyPEM)
		if err != nil {
			return nil, false, fmt.Errorf("CA in %s: %w (delete both files to make a new one)", dir, err)
		}
	case errors.Is(err1, os.ErrNotExist) && errors.Is(err2, os.ErrNotExist):
		if certPEM, keyPEM, err = newCA(); err != nil {
			return nil, false, err
		}
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return nil, false, err
		}
		if err = os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
			return nil, false, err
		}
		if err = os.WriteFile(certPath, certPEM, 0o644); err != nil {
			return nil, false, err
		}
		if ca, err = parseCA(certPEM, keyPEM); err != nil {
			return nil, false, err
		}
		created = true
	case err1 != nil && !errors.Is(err1, os.ErrNotExist):
		return nil, false, err1
	case err2 != nil && !errors.Is(err2, os.ErrNotExist):
		return nil, false, err2
	default:
		return nil, false, fmt.Errorf("CA in %s: only one of %s and %s exists; delete it to make a new CA", dir, CACertFile, CAKeyFile)
	}
	ca.CertPath = certPath
	if ca.leafKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
		return nil, false, err
	}
	return ca, created, nil
}

func newCA() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "lt record CA (" + host + ")", Organization: []string{"lt load testing (local)"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(5, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), nil
}

func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, fmt.Errorf("not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	var key *ecdsa.PrivateKey
	if key, err = x509.ParseECPrivateKey(kb.Bytes); err != nil {
		k, err2 := x509.ParsePKCS8PrivateKey(kb.Bytes)
		var ok bool
		if key, ok = k.(*ecdsa.PrivateKey); err2 != nil || !ok {
			return nil, fmt.Errorf("key: want an ECDSA key: %w", err)
		}
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("%s is not a CA certificate", CACertFile)
	}
	return &CA{Cert: cert, key: key, certDER: cb.Bytes, leaves: map[string]*tls.Certificate{}}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	return n
}

// Pool returns a cert pool holding the CA (for tests and clients).
func (c *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.Cert)
	return p
}

// Leaf returns a certificate for host signed by the CA.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.leaves[host]; ok {
		return l, nil
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: host, Organization: []string{"lt record"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 30),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &c.leafKey.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	l := &tls.Certificate{Certificate: [][]byte{der, c.certDER}, PrivateKey: c.leafKey}
	c.leaves[host] = l
	return l, nil
}
