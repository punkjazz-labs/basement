package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	ManifestAssetName  = LinuxARM64AssetName + ".update.json"
	SignatureAssetName = LinuxARM64AssetName + ".update.sig"
	githubReleasesURL  = "https://api.github.com/repos/punkjazz-labs/basement/releases?per_page=100"
)

var ErrManualUpgradeRequired = errors.New("no signed release supports rollback from the running manager")

type ReleaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type Release struct {
	TagName    string         `json:"tag_name"`
	HTMLURL    string         `json:"html_url"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []ReleaseAsset `json:"assets"`
}

type ReleaseSource interface {
	Releases(context.Context) ([]Release, error)
	Fetch(context.Context, string, int64) ([]byte, error)
}

type Candidate struct {
	Release       Release
	Manifest      Manifest
	ManifestBytes []byte
	Signature     []byte
	AssetURL      string
}

type Resolution struct {
	Candidate        *Candidate
	NewestPublished  string
	NewestReleaseURL string
	ManualUpgrade    bool
}

type Resolver struct {
	Source ReleaseSource
	Keys   KeyRing
}

func (resolver Resolver) Resolve(ctx context.Context, runningVersion string) (Resolution, error) {
	running, err := ParseVersion(runningVersion)
	if err != nil {
		return Resolution{}, errors.New("the running manager is not a stable release")
	}
	if resolver.Source == nil {
		return Resolution{}, errors.New("manager update release source is unavailable")
	}
	if len(resolver.Keys) == 0 {
		return Resolution{}, errors.New("no manager update release key is embedded")
	}
	releases, err := resolver.Source.Releases(ctx)
	if err != nil {
		return Resolution{}, err
	}
	type orderedRelease struct {
		release Release
		version Version
	}
	ordered := make([]orderedRelease, 0, len(releases))
	result := Resolution{}
	for _, release := range releases {
		if release.Draft || release.Prerelease {
			continue
		}
		version, err := ParseVersion(release.TagName)
		if err != nil {
			continue
		}
		if result.NewestPublished == "" {
			result.NewestPublished = release.TagName
			result.NewestReleaseURL = release.HTMLURL
		} else if newest, parseErr := ParseVersion(result.NewestPublished); parseErr == nil && version.Compare(newest) > 0 {
			result.NewestPublished = release.TagName
			result.NewestReleaseURL = release.HTMLURL
		}
		if version.Compare(running) > 0 {
			ordered = append(ordered, orderedRelease{release: release, version: version})
		}
	}
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].version.Compare(ordered[right].version) > 0
	})
	hasSignedNewerRelease := false
	for _, item := range ordered {
		manifestURL, signatureURL, assetURL, ok := updateAssetURLs(item.release.Assets)
		if !ok {
			continue
		}
		manifestBytes, err := resolver.Source.Fetch(ctx, manifestURL, MaxManifestBytes)
		if err != nil {
			continue
		}
		signature, err := resolver.Source.Fetch(ctx, signatureURL, MaxSignatureBytes)
		if err != nil {
			continue
		}
		manifest, err := VerifySignedManifest(manifestBytes, signature, resolver.Keys)
		if err != nil || manifest.ReleaseVersion != item.release.TagName {
			continue
		}
		hasSignedNewerRelease = true
		if err := ValidateCandidate(manifest, item.release.TagName, runningVersion); err != nil {
			continue
		}
		candidate := Candidate{
			Release: item.release, Manifest: manifest, ManifestBytes: append([]byte(nil), manifestBytes...),
			Signature: append([]byte(nil), signature...), AssetURL: assetURL,
		}
		result.Candidate = &candidate
		return result, nil
	}
	result.ManualUpgrade = hasSignedNewerRelease
	return result, nil
}

func updateAssetURLs(assets []ReleaseAsset) (manifestURL, signatureURL, assetURL string, ok bool) {
	for _, asset := range assets {
		switch asset.Name {
		case ManifestAssetName:
			manifestURL = asset.URL
		case SignatureAssetName:
			signatureURL = asset.URL
		case LinuxARM64AssetName:
			assetURL = asset.URL
		}
	}
	return manifestURL, signatureURL, assetURL, manifestURL != "" && signatureURL != "" && assetURL != ""
}

type HTTPReleaseSource struct {
	Client *http.Client
}

func NewHTTPReleaseSource() *HTTPReleaseSource {
	return &HTTPReleaseSource{Client: &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 4 {
				return errors.New("too many release redirects")
			}
			if request.URL.Scheme != "https" || !releaseHostAllowed(request.URL.Hostname()) {
				return errors.New("release redirect left the allowed hosts")
			}
			return nil
		},
	}}
}

func (source *HTTPReleaseSource) Releases(ctx context.Context) ([]Release, error) {
	payload, err := source.Fetch(ctx, githubReleasesURL, 2<<20)
	if err != nil {
		return nil, err
	}
	var releases []Release
	if err := json.Unmarshal(payload, &releases); err != nil {
		return nil, fmt.Errorf("decode release list: %w", err)
	}
	return releases, nil
}

func (source *HTTPReleaseSource) Fetch(ctx context.Context, location string, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("release response limit is invalid")
	}
	parsed, err := url.Parse(location)
	if err != nil || parsed.Scheme != "https" || !releaseHostAllowed(parsed.Hostname()) {
		return nil, errors.New("release asset URL is not allowed")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "basement-manager-update")
	response, err := source.client().Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch release data: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch release data: HTTP %d", response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read release data: %w", err)
	}
	if int64(len(payload)) > limit {
		return nil, errors.New("release response exceeded its size limit")
	}
	return payload, nil
}

func (source *HTTPReleaseSource) client() *http.Client {
	if source != nil && source.Client != nil {
		return source.Client
	}
	return NewHTTPReleaseSource().Client
}

func releaseHostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "api.github.com" || host == "github.com" || host == "objects.githubusercontent.com" || host == "release-assets.githubusercontent.com"
}
