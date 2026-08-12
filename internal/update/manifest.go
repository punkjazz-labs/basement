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
	"os"
	"regexp"
	"sort"
	"strings"
)

const (
	// ManifestSchemaVersion is the base schema: one signed manager asset and
	// nothing else. ManifestHelperSchemaVersion adds the signed identity of
	// the root updater helper, so the helper rides the same signature as the
	// manager (ADR 0020, decision 1).
	ManifestSchemaVersion       = 1
	ManifestHelperSchemaVersion = 2
	// UpdaterProtocol is the highest protocol this build speaks. A protocol-2
	// manager accepts both schemas; a protocol-1 manager refuses schema 2 on
	// its schema constant and on strict unknown-field decoding, which is the
	// transition mechanism rather than a bug to work around.
	UpdaterProtocol           = 2
	LinuxARM64AssetName       = "basement-linux-arm64"
	LinuxARM64HelperAssetName = "basement-updater-linux-arm64"
	MaxManifestBytes          = 64 << 10
	MaxSignatureBytes         = 256
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
	// The helper fields exist only in schema 2 and are appended last so a
	// schema-1 manifest still marshals to exactly the bytes it always did.
	HelperAssetName string `json:"helper_asset_name,omitempty"`
	HelperSize      int64  `json:"helper_size,omitempty"`
	HelperSHA256    string `json:"helper_sha256,omitempty"`
}

// ManifestProtocol reports the updater protocol a schema requires. The
// manifest field is what an older manager checks before it refuses a release
// it cannot apply, so a schema-1 manifest is published at protocol 1 and
// stays reachable from every protocol-1 manager already in the field. Only a
// schema-2 manifest demands protocol 2.
func ManifestProtocol(schema int) (int, error) {
	switch schema {
	case ManifestSchemaVersion:
		return 1, nil
	case ManifestHelperSchemaVersion:
		return 2, nil
	default:
		return 0, errors.New("update manifest schema is unsupported")
	}
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
	requiredProtocol, err := ManifestProtocol(manifest.SchemaVersion)
	if err != nil {
		return err
	}
	switch {
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
	case manifest.UpdaterProtocol < 1 || manifest.UpdaterProtocol > UpdaterProtocol:
		return errors.New("update manifest updater protocol is unsupported")
	case manifest.UpdaterProtocol < requiredProtocol:
		return errors.New("update manifest updater protocol is older than its schema requires")
	}
	if err := validateManifestHelper(manifest); err != nil {
		return err
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

// validateManifestHelper keeps each schema strict about the other's fields. A
// schema-1 manifest that carried a helper digest would claim an evaluation the
// schema does not define, and a schema-2 manifest without one would promise a
// helper it never names.
func validateManifestHelper(manifest Manifest) error {
	if manifest.SchemaVersion == ManifestSchemaVersion {
		if manifest.HelperAssetName != "" || manifest.HelperSize != 0 || manifest.HelperSHA256 != "" {
			return errors.New("update manifest schema 1 does not carry a helper identity")
		}
		return nil
	}
	switch {
	case manifest.HelperAssetName != LinuxARM64HelperAssetName:
		return errors.New("update manifest names an unsupported helper asset")
	case manifest.HelperSize <= 0:
		return errors.New("update manifest helper size is invalid")
	case !hexDigestPattern.MatchString(manifest.HelperSHA256):
		return errors.New("update manifest helper digest is invalid")
	}
	return nil
}

// VerifyHelperAsset applies the manager payload's rules to the root updater
// helper: the signed size exactly, the signed digest, and the same Linux
// ARM64 ELF format check. It reports helper wording so a failure names the
// binary that actually failed.
func VerifyHelperAsset(reader io.Reader, expectedSize int64, expectedDigest string) error {
	if err := VerifyAsset(reader, expectedSize, expectedDigest); err != nil {
		return fmt.Errorf("root updater helper: %w", err)
	}
	return nil
}

// FileDigest reports the SHA-256 of a file already on disk. The manager uses
// it to compare the installed helper against a signed digest, and the helper
// uses it on the bytes actually executing.
func FileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
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
