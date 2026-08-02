// Command sign-index produces the detached signature (index.json.minisig)
// for a recipe index, using a private key that lives only in a file on the
// signer's own machine — never in this repository, never in an environment
// variable's value (only its value is a path), never logged. It is not part
// of the basement binary: the ability to sign an index ships in no binary
// except this standalone tool, run by hand or from a release process that
// has the real key.
//
// Usage:
//
//	go run ./cmd/sign-index -index path/to/index.json -key path/to/private.key
//
// The key file holds the base64 encoding of a raw 64-byte ed25519 private
// key (the exact format crypto/ed25519.GenerateKey returns as its second
// value) — generate one with a short throwaway script, print only its
// public half to cross-check against recipe.IndexPublicKeyBase64, and store
// the private half in the approved OS secret store, never in this repo.
// `make sign-index` wraps this command and requires the key path in
// BASEMENT_SIGN_KEY so it is never typed as a command-line argument visible
// in shell history or a process list.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	indexPath := flag.String("index", "index.json", "path to the index.json to sign")
	keyPath := flag.String("key", "", "path to a file containing the base64 ed25519 private key (required)")
	flag.Parse()
	if *keyPath == "" {
		fmt.Fprintln(os.Stderr, "sign-index: -key is required (path to the private key file; see cmd/sign-index/main.go)")
		os.Exit(2)
	}
	if err := run(*indexPath, *keyPath); err != nil {
		fmt.Fprintln(os.Stderr, "sign-index:", err)
		os.Exit(1)
	}
}

func run(indexPath, keyPath string) error {
	indexBytes, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("read index: %w", err)
	}
	keyFile, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read private key file: %w", err)
	}
	privateKey, err := decodePrivateKey(keyFile)
	if err != nil {
		return fmt.Errorf("decode private key: %w", err)
	}
	signature := ed25519.Sign(privateKey, indexBytes)
	signatureFile := base64.StdEncoding.EncodeToString(signature) + "\n"
	sigPath := indexPath + ".minisig"
	if err := os.WriteFile(sigPath, []byte(signatureFile), 0o640); err != nil {
		return fmt.Errorf("write signature: %w", err)
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	fmt.Printf("wrote %s\n", sigPath)
	fmt.Printf("signed with public key: %s\n", base64.StdEncoding.EncodeToString(publicKey))
	fmt.Println("verify this matches recipe.IndexPublicKeyBase64 in internal/recipe/signature.go before publishing")
	return nil
}

func decodePrivateKey(keyFile []byte) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(keyFile)))
	if err != nil {
		return nil, fmt.Errorf("key file is not valid base64: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("key file has length %d, want %d (a raw ed25519 private key)", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}
