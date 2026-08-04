package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

const (
	ManifestSchemaVersion = 1
	UpdaterProtocol       = 1
	LinuxARM64AssetName   = "basement-linux-arm64"
	MaxManifestBytes      = 64 << 10
	MaxSignatureBytes     = 256
)

var (
	hexDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	keyIDPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

	// releasePublicKeys is set for production binaries with -X. Its format is
	// key-id=base64-public-key, with comma separators for a rotation window.
	// It is intentionally empty in ordinary local builds.
	releasePublicKeys string
)

type Manifest struct {
	SchemaVersion   int      `json:"schema_version"`
	KeyID           string   `json:"key_id"`
	ReleaseVersion  string   `json:"release_version"`
	OS              string   `json:"os"`
	Arch            string   `json:"arch"`
	AssetName       string   `json:"asset_name"`
	AssetSize       int64    `json:"asset_size"`
	AssetSHA256     string   `json:"asset_sha256"`
	UpdaterProtocol int      `json:"updater_protocol"`
	RollbackFrom    []string `json:"rollback_from"`
}

type KeyRing map[string]ed25519.PublicKey

func ProductionKeyRing() (KeyRing, error) {
	return ParseKeyRing(releasePublicKeys)
}

// ValidateKeyID applies the release key identifier rule shared by manifests
// and embedded key rings.
func ValidateKeyID(keyID string) error {
	if !keyIDPattern.MatchString(keyID) {
		return errors.New("manager update key id is invalid")
	}
	return nil
}

func ParseKeyRing(value string) (KeyRing, error) {
	if value == "" {
		return nil, errors.New("no manager update release key is embedded")
	}
	ring := make(KeyRing)
	for _, entry := range strings.Split(value, ",") {
		keyID, encoded, ok := strings.Cut(entry, "=")
		if !ok || !keyIDPattern.MatchString(keyID) || encoded == "" {
			return nil, errors.New("embedded manager update key ring is malformed")
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || len(decoded) != ed25519.PublicKeySize {
			return nil, errors.New("embedded manager update public key is malformed")
		}
		if _, exists := ring[keyID]; exists {
			return nil, errors.New("embedded manager update key id is duplicated")
		}
		ring[keyID] = ed25519.PublicKey(append([]byte(nil), decoded...))
	}
	return ring, nil
}

func EncodeKeyRing(ring KeyRing) (string, error) {
	if len(ring) == 0 {
		return "", errors.New("key ring is empty")
	}
	ids := make([]string, 0, len(ring))
	for keyID, publicKey := range ring {
		if !keyIDPattern.MatchString(keyID) || len(publicKey) != ed25519.PublicKeySize {
			return "", errors.New("key ring entry is malformed")
		}
		ids = append(ids, keyID)
	}
	sort.Strings(ids)
	entries := make([]string, 0, len(ids))
	for _, keyID := range ids {
		entries = append(entries, keyID+"="+base64.StdEncoding.EncodeToString(ring[keyID]))
	}
	return strings.Join(entries, ","), nil
}

func MarshalManifest(manifest Manifest) ([]byte, error) {
	if err := validateManifest(manifest); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func VerifySignedManifest(manifestBytes, signatureBytes []byte, ring KeyRing) (Manifest, error) {
	if len(manifestBytes) == 0 || len(manifestBytes) > MaxManifestBytes {
		return Manifest{}, errors.New("update manifest size is invalid")
	}
	var selector struct {
		KeyID string `json:"key_id"`
	}
	if err := json.Unmarshal(manifestBytes, &selector); err != nil || selector.KeyID == "" {
		return Manifest{}, errors.New("update manifest cannot select a release key")
	}
	publicKey, ok := ring[selector.KeyID]
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return Manifest{}, errors.New("update manifest names an unknown release key")
	}
	signature, err := decodeSignature(signatureBytes)
	if err != nil {
		return Manifest{}, err
	}
	if !ed25519.Verify(publicKey, manifestBytes, signature) {
		return Manifest{}, errors.New("update manifest signature is invalid")
	}
	manifest, err := decodeManifest(manifestBytes)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func ValidateCandidate(manifest Manifest, releaseTag, runningVersion string) error {
	if manifest.ReleaseVersion != releaseTag {
		return errors.New("release tag does not match the signed update manifest")
	}
	running, err := ParseVersion(runningVersion)
	if err != nil {
		return errors.New("the running manager is not a stable release")
	}
	target, err := ParseVersion(manifest.ReleaseVersion)
	if err != nil {
		return errors.New("the signed target is not a stable release")
	}
	if target.Compare(running) <= 0 {
		return errors.New("the signed target is not newer than the running manager")
	}
	for _, allowed := range manifest.RollbackFrom {
		if allowed == runningVersion {
			return nil
		}
	}
	return errors.New("the signed release does not support rollback from the running manager")
}

func ManifestDigest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func VerifyAsset(reader io.Reader, expectedSize int64, expectedDigest string) error {
	if expectedSize <= 0 || !hexDigestPattern.MatchString(expectedDigest) {
		return errors.New("signed asset identity is invalid")
	}
	hasher := sha256.New()
	header := make([]byte, 20)
	count, err := io.Copy(io.MultiWriter(hasher, &headerWriter{buffer: header}), io.LimitReader(reader, expectedSize+1))
	if err != nil {
		return fmt.Errorf("read manager update asset: %w", err)
	}
	if count != expectedSize {
		return errors.New("manager update asset size does not match its signed manifest")
	}
	if hex.EncodeToString(hasher.Sum(nil)) != expectedDigest {
		return errors.New("manager update asset digest does not match its signed manifest")
	}
	if err := ValidateARM64ELF(header); err != nil {
		return err
	}
	return nil
}

func ValidateARM64ELF(header []byte) error {
	if len(header) < 20 || !bytes.Equal(header[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return errors.New("manager update asset is not a Linux ELF binary")
	}
	if machine := uint16(header[18]) | uint16(header[19])<<8; machine != 183 {
		return errors.New("manager update asset is not an ARM64 build")
	}
	return nil
}

func decodeManifest(payload []byte) (Manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("update manifest is invalid: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("update manifest must contain exactly one JSON object")
	}
	return manifest, nil
}

func decodeSignature(payload []byte) ([]byte, error) {
	if len(payload) == 0 || len(payload) > MaxSignatureBytes || payload[len(payload)-1] != '\n' || bytes.Count(payload, []byte{'\n'}) != 1 || bytes.Contains(payload, []byte{'\r'}) {
		return nil, errors.New("update manifest signature must be one base64 line")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(string(payload[:len(payload)-1]))
	if err != nil || len(decoded) != ed25519.SignatureSize {
		return nil, errors.New("update manifest signature is malformed")
	}
	return decoded, nil
}

func validateManifest(manifest Manifest) error {
	switch {
	case manifest.SchemaVersion != ManifestSchemaVersion:
		return errors.New("update manifest schema is unsupported")
	case !keyIDPattern.MatchString(manifest.KeyID):
		return errors.New("update manifest key id is invalid")
	case manifest.OS != "linux" || manifest.Arch != "arm64":
		return errors.New("update manifest is for a different platform")
	case manifest.AssetName != LinuxARM64AssetName:
		return errors.New("update manifest names an unsupported asset")
	case manifest.AssetSize <= 0:
		return errors.New("update manifest asset size is invalid")
	case !hexDigestPattern.MatchString(manifest.AssetSHA256):
		return errors.New("update manifest asset digest is invalid")
	case manifest.UpdaterProtocol != UpdaterProtocol:
		return errors.New("update manifest updater protocol is unsupported")
	}
	if _, err := ParseVersion(manifest.ReleaseVersion); err != nil {
		return errors.New("update manifest release version is invalid")
	}
	if len(manifest.RollbackFrom) == 0 {
		return errors.New("update manifest has no rollback window")
	}
	seen := make(map[string]bool, len(manifest.RollbackFrom))
	for _, version := range manifest.RollbackFrom {
		if _, err := ParseVersion(version); err != nil {
			return errors.New("update manifest rollback version is invalid")
		}
		if seen[version] {
			return errors.New("update manifest rollback version is duplicated")
		}
		seen[version] = true
	}
	return nil
}

type headerWriter struct {
	buffer []byte
	offset int
}

func (writer *headerWriter) Write(payload []byte) (int, error) {
	if writer.offset < len(writer.buffer) {
		writer.offset += copy(writer.buffer[writer.offset:], payload)
	}
	return len(payload), nil
}
