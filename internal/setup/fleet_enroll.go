package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxFleetEnrollmentBody = 1 << 20

var fleetHTTPClient = &http.Client{
	Timeout: 20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

type consoleAuthority struct {
	Cookie string
	CSRF   string
}

// EnrollInstalledFleet turns one owner-approved multi-target setup run into
// one controller and up to three members. Every target already received the
// same binary through Install before this starts. The pairing tokens are used
// only against each node's fixed console endpoints and are never placed in an
// error, progress line, database row, or fleet request.
func EnrollInstalledFleet(ctx context.Context, installed []Machine) error {
	if len(installed) < 2 {
		return nil
	}
	if len(installed) > 4 {
		return errors.New("one setup run can enroll at most four nodes")
	}
	controller := installed[0]
	controllerAuthority, err := pairInstalledConsole(ctx, controller.Result.ConsoleURL, controller.Result.Token)
	if err != nil {
		return fmt.Errorf("open the controller console: %w", err)
	}
	for _, member := range installed[1:] {
		memberAuthority, err := pairInstalledConsole(ctx, member.Result.ConsoleURL, member.Result.Token)
		if err != nil {
			return fmt.Errorf("open the installed member console: %w", err)
		}
		var joinCode struct {
			Code      string `json:"code"`
			ExpiresAt string `json:"expires_at"`
		}
		if err := callInstalledConsole(ctx, http.MethodPost, member.Result.ConsoleURL, "/api/v1/fleet/join-code", memberAuthority, nil, &joinCode); err != nil {
			return fmt.Errorf("create the installed member join code: %w", err)
		}
		if joinCode.Code == "" {
			return errors.New("the installed member returned an empty fleet join code")
		}
		nodeURL, err := adjacentNodeURL(member.Result.ConsoleURL)
		if err != nil {
			return err
		}
		request := struct {
			DisplayName string `json:"display_name"`
			ConsoleURL  string `json:"console_url"`
			NodeURL     string `json:"node_url"`
			JoinCode    string `json:"join_code"`
		}{DisplayName: member.Target, ConsoleURL: member.Result.ConsoleURL, NodeURL: nodeURL, JoinCode: joinCode.Code}
		joined := map[string]json.RawMessage{}
		if err := callInstalledConsole(ctx, http.MethodPost, controller.Result.ConsoleURL, "/api/v1/fleet/join", controllerAuthority, request, &joined); err != nil {
			return fmt.Errorf("enroll %s from the controller: %w", member.Target, err)
		}
		if len(joined["node"]) == 0 {
			return errors.New("the controller did not confirm the enrolled node")
		}
	}
	return nil
}

func pairInstalledConsole(ctx context.Context, consoleURL, token string) (consoleAuthority, error) {
	if strings.TrimSpace(token) == "" {
		return consoleAuthority{}, errors.New("the installed manager did not return a pairing token")
	}
	body := struct {
		Token string `json:"token"`
	}{Token: token}
	var paired struct {
		CSRF string `json:"csrf_token"`
	}
	response, err := installedConsoleRequest(ctx, http.MethodPost, consoleURL, "/api/v1/auth/pair", consoleAuthority{}, body)
	if err != nil {
		return consoleAuthority{}, err
	}
	defer response.Body.Close()
	payload, err := readEnrollmentResponse(response)
	if err != nil {
		return consoleAuthority{}, err
	}
	if err := json.Unmarshal(payload, &paired); err != nil || paired.CSRF == "" {
		return consoleAuthority{}, errors.New("the installed manager did not return console authority")
	}
	cookies := response.Header.Values("Set-Cookie")
	if len(cookies) == 0 {
		return consoleAuthority{}, errors.New("the installed manager did not open a console session")
	}
	var values []string
	for _, raw := range cookies {
		value, _, _ := strings.Cut(raw, ";")
		if value != "" {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return consoleAuthority{}, errors.New("the installed manager returned an unreadable console session")
	}
	return consoleAuthority{Cookie: strings.Join(values, "; "), CSRF: paired.CSRF}, nil
}

func callInstalledConsole(ctx context.Context, method, consoleURL, endpoint string, authority consoleAuthority, body, target any) error {
	response, err := installedConsoleRequest(ctx, method, consoleURL, endpoint, authority, body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, err := readEnrollmentResponse(response)
	if err != nil {
		return err
	}
	if target == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("the installed manager returned an unreadable response")
	}
	return nil
}

func installedConsoleRequest(ctx context.Context, method, consoleURL, endpoint string, authority consoleAuthority, body any) (*http.Response, error) {
	origin, err := url.Parse(strings.TrimSuffix(consoleURL, "/"))
	if err != nil || origin.Host == "" || (origin.Scheme != "http" && origin.Scheme != "https") || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return nil, errors.New("an installed manager returned an invalid console URL")
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(origin.String(), "/")+endpoint, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Origin", strings.TrimSuffix(origin.String(), "/"))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authority.Cookie != "" {
		request.Header.Set("Cookie", authority.Cookie)
		request.Header.Set("X-CSRF-Token", authority.CSRF)
	}
	return fleetHTTPClient.Do(request)
}

func readEnrollmentResponse(response *http.Response) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxFleetEnrollmentBody+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxFleetEnrollmentBody {
		return nil, errors.New("the installed manager response exceeds the size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &failure) == nil && failure.Error != "" {
			return nil, errors.New(failure.Error)
		}
		return nil, fmt.Errorf("the installed manager returned status %d", response.StatusCode)
	}
	return payload, nil
}

func adjacentNodeURL(consoleURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSuffix(consoleURL, "/"))
	if err != nil || parsed.Host == "" {
		return "", errors.New("an installed manager returned an invalid console URL")
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return "", errors.New("the installed manager console URL has no explicit port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port >= 65535 {
		return "", errors.New("the installed manager console port cannot address its fleet listener")
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(port+1)), nil
}
