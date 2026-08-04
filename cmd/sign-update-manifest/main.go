package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/punkjazz-labs/basement/internal/update"
)

func main() {
	mode := flag.String("mode", "sign", "sign, verify, or keygen")
	assetPath := flag.String("asset", "", "linux ARM64 manager binary")
	version := flag.String("version", "", "release tag")
	keyID := flag.String("key-id", "", "release signing key id")
	rollbackFrom := flag.String("rollback-from", "", "comma-separated rollback versions")
	manifestPath := flag.String("manifest", "", "manifest output or input path")
	signaturePath := flag.String("signature", "", "signature output or input path")
	publicKeyPath := flag.String("public-key-out", "", "public key ring entry output path")
	keyRingValue := flag.String("key-ring", "", "public key ring used for verification")
	flag.Parse()

	var err error
	switch *mode {
	case "sign":
		err = sign(*assetPath, *version, *keyID, *rollbackFrom, *manifestPath, *signaturePath, *publicKeyPath, os.Stdin)
	case "verify":
		err = verify(*assetPath, *version, *manifestPath, *signaturePath, *keyRingValue)
	case "keygen":
		err = keygen(*keyID, *publicKeyPath, os.Stdout)
	default:
		err = errors.New("mode must be sign, verify, or keygen")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func sign(assetPath, version, keyID, rollbackCSV, manifestPath, signaturePath, publicKeyPath string, privateKeyInput io.Reader) error {
	if assetPath == "" || version == "" || keyID == "" || rollbackCSV == "" || manifestPath == "" || signaturePath == "" || publicKeyPath == "" {
		return errors.New("sign requires asset, version, key-id, rollback-from, manifest, signature, and public-key-out")
	}
	privateKey, err := readPrivateKey(privateKeyInput)
	if err != nil {
		return err
	}
	asset, err := os.Open(assetPath)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	size, err := io.Copy(hasher, asset)
	closeErr := asset.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	rollback := strings.Split(rollbackCSV, ",")
	manifestBytes, err := update.MarshalManifest(update.Manifest{
		SchemaVersion: update.ManifestSchemaVersion, KeyID: keyID, ReleaseVersion: version,
		OS: "linux", Arch: "arm64", AssetName: update.LinuxARM64AssetName,
		AssetSize: size, AssetSHA256: hex.EncodeToString(hasher.Sum(nil)), UpdaterProtocol: update.UpdaterProtocol,
		RollbackFrom: rollback,
	})
	if err != nil {
		return err
	}
	signature := ed25519.Sign(privateKey, manifestBytes)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	if !ed25519.Verify(publicKey, manifestBytes, signature) {
		return errors.New("generated update signature did not verify")
	}
	ringValue, err := update.EncodeKeyRing(update.KeyRing{keyID: publicKey})
	if err != nil {
		return err
	}
	if err := os.WriteFile(manifestPath, manifestBytes, 0o644); err != nil {
		return err
	}
	detached := append([]byte(base64.StdEncoding.EncodeToString(signature)), '\n')
	if err := os.WriteFile(signaturePath, detached, 0o644); err != nil {
		return err
	}
	return os.WriteFile(publicKeyPath, append([]byte(ringValue), '\n'), 0o644)
}

func keygen(keyID, publicKeyPath string, privateKeyOutput io.Writer) error {
	if keyID == "" || publicKeyPath == "" {
		return errors.New("keygen requires key-id and public-key-out")
	}
	if outputFile, ok := privateKeyOutput.(*os.File); ok {
		info, err := outputFile.Stat()
		if err != nil {
			return errors.New("inspect update signing private key output")
		}
		if info.Mode().IsRegular() {
			return errors.New("keygen refuses to write the private key to a file; pipe stdout directly to Keychain")
		}
	}
	if err := update.ValidateKeyID(keyID); err != nil {
		return err
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("generate update signing key")
	}
	ringValue, err := update.EncodeKeyRing(update.KeyRing{keyID: publicKey})
	if err != nil {
		return err
	}
	if err := os.WriteFile(publicKeyPath, append([]byte(ringValue), '\n'), 0o644); err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(privateKey) + "\n"
	written, err := io.WriteString(privateKeyOutput, encoded)
	if err != nil {
		return errors.New("write update signing private key to stdout")
	}
	if written != len(encoded) {
		return io.ErrShortWrite
	}
	return nil
}

func verify(assetPath, releaseTag, manifestPath, signaturePath, keyRingValue string) error {
	if assetPath == "" || releaseTag == "" || manifestPath == "" || signaturePath == "" || keyRingValue == "" {
		return errors.New("verify requires asset, version, manifest, signature, and key-ring")
	}
	ring, err := update.ParseKeyRing(keyRingValue)
	if err != nil {
		return err
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	signature, err := os.ReadFile(signaturePath)
	if err != nil {
		return err
	}
	manifest, err := update.VerifySignedManifest(manifestBytes, signature, ring)
	if err != nil {
		return err
	}
	if manifest.ReleaseVersion != releaseTag {
		return errors.New("release tag does not match the signed manifest")
	}
	asset, err := os.Open(assetPath)
	if err != nil {
		return err
	}
	verifyErr := update.VerifyAsset(asset, manifest.AssetSize, manifest.AssetSHA256)
	if closeErr := asset.Close(); verifyErr == nil {
		verifyErr = closeErr
	}
	return verifyErr
}

func readPrivateKey(reader io.Reader) (ed25519.PrivateKey, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, 2049))
	if err != nil {
		return nil, errors.New("read update signing key")
	}
	encoded := strings.TrimSpace(string(payload))
	if encoded == "" || strings.ContainsAny(encoded, " \t\r\n") {
		return nil, errors.New("update signing key must be one base64 value on stdin")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, errors.New("update signing key is not valid base64")
	}
	switch len(decoded) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(decoded), nil
	case ed25519.PrivateKeySize:
		privateKey := ed25519.PrivateKey(append([]byte(nil), decoded...))
		expected := ed25519.NewKeyFromSeed(privateKey[:ed25519.SeedSize])
		if !privateKey.Equal(expected) {
			return nil, errors.New("update signing private key is inconsistent")
		}
		return privateKey, nil
	default:
		return nil, errors.New("update signing key has the wrong length")
	}
}
