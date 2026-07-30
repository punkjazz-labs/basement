package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const cookieName = "runonspark_session"

type Manager struct {
	pairingToken string
	signingKey   []byte
	tokenPath    string
}

func Open(dataDir string) (*Manager, error) {
	tokenPath := filepath.Join(dataDir, "pairing-token")
	token, err := secretFile(tokenPath, 24)
	if err != nil {
		return nil, fmt.Errorf("pairing token: %w", err)
	}
	key, err := secretFile(filepath.Join(dataDir, "auth-signing-key"), 32)
	if err != nil {
		return nil, fmt.Errorf("auth signing key: %w", err)
	}
	return &Manager{pairingToken: token, signingKey: []byte(key), tokenPath: tokenPath}, nil
}

func (m *Manager) PairingTokenPath() string { return m.tokenPath }

func (m *Manager) Pair(w http.ResponseWriter, supplied string) (string, error) {
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(m.pairingToken)) != 1 {
		return "", errors.New("invalid pairing token")
	}
	csrf, err := randomToken(24)
	if err != nil {
		return "", err
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	payload := csrf + "." + timestamp
	signature := m.sign(payload)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: payload + "." + signature, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 30 * 24 * 60 * 60})
	return csrf, nil
}

func (m *Manager) Authenticate(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return "", false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(parts[2]), []byte(m.sign(payload))) {
		return "", false
	}
	issued, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Since(time.Unix(issued, 0)) > 30*24*time.Hour {
		return "", false
	}
	return parts[0], true
}

func (m *Manager) AuthorizeMutation(r *http.Request) error {
	csrf, ok := m.Authenticate(r)
	if !ok {
		return errors.New("authentication required")
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf)) != 1 {
		return errors.New("valid CSRF token required")
	}
	if err := ValidateOrigin(r); err != nil {
		return err
	}
	return nil
}

func (m *Manager) sign(payload string) string {
	mac := hmac.New(sha256.New, m.signingKey)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func ValidateOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return errors.New("origin header is required")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("cross-origin mutation denied")
	}
	return nil
}

func secretFile(path string, bytes int) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		value := strings.TrimSpace(string(data))
		if value == "" {
			return "", errors.New("secret file is empty")
		}
		return value, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", err
	}
	value, err := randomToken(bytes)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return secretFile(path, bytes)
		}
		return "", err
	}
	if _, err := f.WriteString(value + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return value, nil
}

func randomToken(bytes int) (string, error) {
	data := make([]byte, bytes)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
