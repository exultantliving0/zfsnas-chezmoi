package certgen

import (
	"archive/zip"
	"bytes"
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
	"strings"
	"time"
)

// Generate creates a self-signed ECDSA TLS certificate valid for 10 years.
func Generate(certFile, keyFile string) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	template := &x509.Certificate{
		SerialNumber: func() *big.Int { n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)); return n }(),
		Subject: pkix.Name{
			Organization: []string{"ZFS NAS Portal"},
			CommonName:   "zfsnas",
		},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost", "zfsnas"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return err
	}

	certOut, err := os.Create(certFile)
	if err != nil {
		return err
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		return err
	}

	keyOut, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer keyOut.Close()

	privDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	return pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER})
}

// Exists returns true if both cert and key files exist.
func Exists(certFile, keyFile string) bool {
	_, errCert := os.Stat(certFile)
	_, errKey := os.Stat(keyFile)
	return errCert == nil && errKey == nil
}

// CertInfo holds metadata about a certificate pair in the certs directory.
type CertInfo struct {
	Name         string    `json:"name"`
	CommonName   string    `json:"common_name"`
	SANs         []string  `json:"sans"`
	Issuer       string    `json:"issuer"`
	NotBefore    time.Time `json:"not_before"`
	NotAfter     time.Time `json:"not_after"`
	IsExpired    bool      `json:"is_expired"`
	IsValid      bool      `json:"is_valid"`
	IsSelfSigned bool      `json:"is_self_signed"`
	IsActive     bool      `json:"is_active"`
	HasKeyFile   bool      `json:"has_key_file"`
}

// ListCerts returns metadata for all certificate pairs in certsDir.
func ListCerts(certsDir, activeName string) ([]CertInfo, error) {
	entries, err := os.ReadDir(certsDir)
	if err != nil {
		return nil, err
	}

	// Collect all .crt stems.
	stems := make(map[string]bool)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".crt") {
			stems[strings.TrimSuffix(e.Name(), ".crt")] = true
		}
	}

	var result []CertInfo
	for name := range stems {
		certPath := filepath.Join(certsDir, name+".crt")
		keyPath := filepath.Join(certsDir, name+".key")

		info := CertInfo{Name: name}
		_, err := os.Stat(keyPath)
		info.HasKeyFile = err == nil

		certData, err := os.ReadFile(certPath)
		if err != nil {
			result = append(result, info)
			continue
		}

		block, _ := pem.Decode(certData)
		if block == nil {
			result = append(result, info)
			continue
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			result = append(result, info)
			continue
		}

		info.CommonName = cert.Subject.CommonName
		info.Issuer = cert.Issuer.CommonName
		info.NotBefore = cert.NotBefore
		info.NotAfter = cert.NotAfter
		info.IsExpired = time.Now().After(cert.NotAfter)
		info.IsSelfSigned = cert.Issuer.String() == cert.Subject.String()

		// Collect SANs
		for _, ip := range cert.IPAddresses {
			info.SANs = append(info.SANs, ip.String())
		}
		info.SANs = append(info.SANs, cert.DNSNames...)

		if info.HasKeyFile {
			valid, _ := ValidateCertPair(certPath, keyPath)
			info.IsValid = valid
		}

		if activeName == "" {
			info.IsActive = name == "self-signed"
		} else {
			info.IsActive = name == activeName
		}

		result = append(result, info)
	}
	return result, nil
}

// ValidateCertPair checks that the public key in the cert matches the private key.
func ValidateCertPair(certPath, keyPath string) (bool, error) {
	_, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return false, err
	}
	return true, nil
}

// ImportCert writes certBytes to <name>.crt and keyBytes to <name>.key in certsDir.
// Returns an error if the pair does not validate.
func ImportCert(certsDir, name string, certBytes, keyBytes []byte) error {
	certPath := filepath.Join(certsDir, name+".crt")
	keyPath := filepath.Join(certsDir, name+".key")

	// Write to temp files first, validate, then move.
	tmpCert, err := os.CreateTemp(certsDir, "import-*.crt")
	if err != nil {
		return err
	}
	defer os.Remove(tmpCert.Name())
	if _, err := tmpCert.Write(certBytes); err != nil {
		tmpCert.Close()
		return err
	}
	tmpCert.Close()

	tmpKey, err := os.CreateTemp(certsDir, "import-*.key")
	if err != nil {
		return err
	}
	defer os.Remove(tmpKey.Name())
	if err := tmpKey.Chmod(0600); err != nil {
		tmpKey.Close()
		return err
	}
	if _, err := tmpKey.Write(keyBytes); err != nil {
		tmpKey.Close()
		return err
	}
	tmpKey.Close()

	// Validate the pair
	if valid, err := ValidateCertPair(tmpCert.Name(), tmpKey.Name()); !valid {
		if err != nil {
			return fmt.Errorf("certificate and key do not match: %w", err)
		}
		return errors.New("certificate and key do not match")
	}

	if err := os.Rename(tmpCert.Name(), certPath); err != nil {
		return err
	}
	if err := os.Rename(tmpKey.Name(), keyPath); err != nil {
		return err
	}
	if err := os.Chmod(keyPath, 0600); err != nil {
		return err
	}
	return nil
}

// ExportCertZip returns a zip archive containing <name>.crt and <name>.key.
func ExportCertZip(certsDir, name string) ([]byte, error) {
	certPath := filepath.Join(certsDir, name+".crt")
	keyPath := filepath.Join(certsDir, name+".key")

	certData, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read cert: %w", err)
	}
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}

	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)

	for _, pair := range []struct{ n string; d []byte }{{name + ".crt", certData}, {name + ".key", keyData}} {
		f, err := zw.Create(pair.n)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(pair.d); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

