package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// hfClient reads the two Hugging Face API endpoints this tool needs: a
// repository's current main revision, and one revision's file listing with
// sizes. It never writes anything and never sees a token, because every
// artifact this pack pins is public.
type hfClient struct {
	base   string
	client *http.Client
}

func newHFClient(base string) *hfClient {
	return &hfClient{
		base:   strings.TrimRight(base, "/"),
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// hfModelInfo is the subset of `GET /api/models/<repository>` this tool
// reads: sha is the current main revision, and the licence is read from
// cardData when present, falling back to the first "license:" tag, because
// both are real shapes the live API returns depending on how a repository's
// card was authored.
type hfModelInfo struct {
	SHA      string     `json:"sha"`
	Tags     []string   `json:"tags"`
	CardData hfCardData `json:"cardData"`
}

type hfCardData struct {
	License string `json:"license"`
}

// licenceTag reports the licence Hugging Face's own API attributes to a
// repository right now, or "" when neither shape carries one.
func (info hfModelInfo) licenceTag() string {
	if info.CardData.License != "" {
		return info.CardData.License
	}
	for _, tag := range info.Tags {
		if value, ok := strings.CutPrefix(tag, "license:"); ok {
			return value
		}
	}
	return ""
}

// hfSibling is one file inside an hfRevisionInfo's tree.
type hfSibling struct {
	RFilename string `json:"rfilename"`
	Size      int64  `json:"size"`
}

// hfRevisionInfo is `GET /api/models/<repository>/revision/<sha>?blobs=true`:
// one revision's complete file listing with sizes, which is where the total
// snapshot size and any pinned file's live size and existence come from.
type hfRevisionInfo struct {
	SHA      string      `json:"sha"`
	Siblings []hfSibling `json:"siblings"`
}

func (info hfRevisionInfo) totalBytes() int64 {
	var total int64
	for _, sibling := range info.Siblings {
		total += sibling.Size
	}
	return total
}

func (info hfRevisionInfo) sibling(name string) (hfSibling, bool) {
	for _, sibling := range info.Siblings {
		if sibling.RFilename == name {
			return sibling, true
		}
	}
	return hfSibling{}, false
}

// hasLicenseFile reports whether this revision's tree carries a verbatim
// LICENSE or LICENSE.md file. It is not a licence check by itself, only a
// fact bump.go's whole-snapshot rule reads to require that a new revision
// still ships whichever of the two the old revision did.
func (info hfRevisionInfo) hasLicenseFile() bool {
	if _, ok := info.sibling("LICENSE"); ok {
		return true
	}
	_, ok := info.sibling("LICENSE.md")
	return ok
}

// ModelInfo fetches a repository's current state.
func (c *hfClient) ModelInfo(repository string) (hfModelInfo, error) {
	var info hfModelInfo
	if err := c.getJSON(c.base+"/api/models/"+repository, &info); err != nil {
		return hfModelInfo{}, fmt.Errorf("hugging face model info for %s: %w", repository, err)
	}
	if info.SHA == "" {
		return hfModelInfo{}, fmt.Errorf("hugging face model info for %s: response carries no sha", repository)
	}
	return info, nil
}

// RevisionInfo fetches one revision's file listing with sizes.
func (c *hfClient) RevisionInfo(repository, revision string) (hfRevisionInfo, error) {
	var info hfRevisionInfo
	url := c.base + "/api/models/" + repository + "/revision/" + revision + "?blobs=true"
	if err := c.getJSON(url, &info); err != nil {
		return hfRevisionInfo{}, fmt.Errorf("hugging face revision info for %s at %s: %w", repository, revision, err)
	}
	return info, nil
}

func (c *hfClient) getJSON(url string, out any) error {
	return getJSON(c.client, url, out)
}

func getJSON(client *http.Client, url string, out any) error {
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", response.StatusCode, url)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("decode response: " + err.Error())
	}
	return nil
}
