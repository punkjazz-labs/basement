package fleet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/punkjazz-labs/basement/internal/store"
)

const identityFileName = "fleet-identity.pem"

type Identity struct {
	NodeID                 string
	PrivateKey             ed25519.PrivateKey
	PublicKey              ed25519.PublicKey
	Certificate            *x509.Certificate
	CertificatePEM         []byte
	CertificateFingerprint string
	tlsCertificate         tls.Certificate
}

// OpenIdentity creates one address-independent node identity and couples its
// public half to the manager database. The node id lives in the certificate
// and the protected file, not in a hostname or route, so DHCP and management
// address changes cannot change who the manager is.
func OpenIdentity(ctx context.Context, dataDir string, database *store.Store) (*Identity, error) {
	path := filepath.Join(dataDir, identityFileName)
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		payload, err = createIdentityFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("open fleet identity: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect fleet identity: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("fleet identity key permissions allow access outside its owner")
	}
	identity, err := parseIdentity(payload)
	if err != nil {
		return nil, fmt.Errorf("parse fleet identity: %w", err)
	}
	if err := database.EnsureNodeIdentity(ctx, store.NodeIdentity{
		NodeID: identity.NodeID, PublicKey: append([]byte(nil), identity.PublicKey...),
		CertificateFingerprint: identity.CertificateFingerprint,
		CreatedAt:              identity.Certificate.NotBefore.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return nil, err
	}
	return identity, nil
}

func createIdentityFile(path string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	nodeID := "node_" + hex.EncodeToString(idBytes)
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, err
	}
	createdAt := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: nodeID},
		NotBefore:             createdAt.Add(-5 * time.Minute),
		NotAfter:              time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		return nil, err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	payload := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})...)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(payload); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return payload, nil
}

func parseIdentity(payload []byte) (*Identity, error) {
	certificatePEM, privatePEM, rest := splitIdentityPEM(payload)
	if len(certificatePEM) == 0 || len(privatePEM) == 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("fleet identity file must contain one certificate and one private key")
	}
	pair, err := tls.X509KeyPair(certificatePEM, privatePEM)
	if err != nil {
		return nil, err
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	privateKey, ok := pair.PrivateKey.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("fleet identity private key is not Ed25519")
	}
	details, err := inspectCertificate(certificate)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(details.PublicKey, privateKey.Public().(ed25519.PublicKey)) {
		return nil, errors.New("fleet identity certificate and private key do not match")
	}
	return &Identity{
		NodeID: details.NodeID, PrivateKey: privateKey, PublicKey: details.PublicKey,
		Certificate: certificate, CertificatePEM: certificatePEM,
		CertificateFingerprint: details.Fingerprint, tlsCertificate: pair,
	}, nil
}

func splitIdentityPEM(payload []byte) (certificate, private, rest []byte) {
	rest = payload
	for len(rest) > 0 {
		block, remaining := pem.Decode(rest)
		if block == nil {
			return certificate, private, rest
		}
		encoded := pem.EncodeToMemory(block)
		switch block.Type {
		case "CERTIFICATE":
			if certificate != nil {
				return nil, nil, payload
			}
			certificate = encoded
		case "PRIVATE KEY":
			if private != nil {
				return nil, nil, payload
			}
			private = encoded
		default:
			return nil, nil, payload
		}
		rest = remaining
	}
	return certificate, private, rest
}

type certificateDetails struct {
	NodeID      string
	PublicKey   ed25519.PublicKey
	Fingerprint string
}

func inspectCertificate(certificate *x509.Certificate) (certificateDetails, error) {
	publicKey, ok := certificate.PublicKey.(ed25519.PublicKey)
	if !ok {
		return certificateDetails{}, errors.New("fleet certificate is not Ed25519")
	}
	if err := certificate.CheckSignature(certificate.SignatureAlgorithm, certificate.RawTBSCertificate, certificate.Signature); err != nil {
		return certificateDetails{}, errors.New("fleet certificate is not self-signed")
	}
	nodeID := strings.TrimSpace(certificate.Subject.CommonName)
	if !validNodeID(nodeID) {
		return certificateDetails{}, errors.New("fleet certificate has an invalid node id")
	}
	digest := sha256.Sum256(certificate.Raw)
	return certificateDetails{NodeID: nodeID, PublicKey: publicKey, Fingerprint: hex.EncodeToString(digest[:])}, nil
}

func validNodeID(nodeID string) bool {
	if len(nodeID) != len("node_")+32 || !strings.HasPrefix(nodeID, "node_") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(nodeID, "node_"))
	return err == nil
}

func ParseCertificatePEM(payload []byte) (*x509.Certificate, certificateDetails, error) {
	block, rest := pem.Decode(payload)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, certificateDetails{}, errors.New("fleet certificate is not one PEM certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, certificateDetails{}, err
	}
	details, err := inspectCertificate(certificate)
	return certificate, details, err
}

func (i *Identity) TLSCertificate() tls.Certificate { return i.tlsCertificate }

func (i *Identity) Sign(payload []byte) []byte {
	return ed25519.Sign(i.PrivateKey, payload)
}

func VerifySignature(publicKey ed25519.PublicKey, payload, signature []byte) bool {
	return ed25519.Verify(publicKey, payload, signature)
}
