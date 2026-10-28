package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
	"unicode"
)

const (
	// DefaultBaseURL is the GitHub REST API.
	DefaultBaseURL = "https://api.github.com"
	// DefaultRepo is the repository releases come from.
	DefaultRepo = "cfcromn/Throt"

	// releasesPerPage keeps the release listing small; the newest
	// releases come first.
	releasesPerPage = 20
	maxReleasesJSON = 4 << 20
	maxChecksums    = 1 << 20
	// MaxArchive caps a downloaded release archive.
	MaxArchive = 100 << 20
	// requestTimeout bounds each HTTP request, the body included.
	requestTimeout = 2 * time.Minute
	maxRedirects   = 5
	ghTimeout      = 5 * time.Second
)

// Release is the part of a GitHub release this package reads.
type Release struct {
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	HTMLURL    string  `json:"html_url"`
	Assets     []Asset `json:"assets"`
}

// Asset is a release asset. URL is its API URL, which serves the file
// itself with Accept: application/octet-stream.
type Asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

// Runner runs a program with arguments and returns its standard output.
// The context carries the timeout.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs programs with os/exec.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// NewHTTPClient returns a client with a whole-request timeout that follows
// at most a few redirects, all over HTTPS. Go drops the Authorization
// header when a redirect leaves the original host, so the token is never
// sent to the asset storage host that release downloads redirect to.
func NewHTTPClient() *http.Client {
	return &http.Client{Timeout: requestTimeout, CheckRedirect: checkRedirect}
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-HTTPS %s", req.URL.Redacted())
	}
	return nil
}

// Client reads releases of one GitHub repository.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	Repo    string
	// Token authenticates requests when set. It is never logged or put in
	// an error.
	Token string
}

// errNotFound is a 404 from GitHub.
var errNotFound = errors.New("not found")

// get fetches u with the given Accept header and returns at most limit
// bytes of body; a longer body is an error.
func (c *Client) get(ctx context.Context, u, accept string, limit int64) ([]byte, error) {
	pu, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	if pu.Scheme != "https" {
		return nil, fmt.Errorf("refusing non-HTTPS URL %s", pu.Redacted())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "throt-update")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = NewHTTPClient()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNotFound
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("GitHub rejected the token (401); check GH_TOKEN, GITHUB_TOKEN or `gh auth status`")
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("GitHub refused the request (%s); anonymous requests are rate limited, so set GH_TOKEN or run `gh auth login`", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub returned %s for %s", resp.Status, pu.Path)
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("%s is %d bytes, more than the %d allowed", pu.Path, resp.ContentLength, limit)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s is more than the %d bytes allowed", pu.Path, limit)
	}
	return body, nil
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) repo() string {
	if c.Repo == "" {
		return DefaultRepo
	}
	return c.Repo
}

// Releases lists the newest releases, drafts included as GitHub returns
// them (Select skips drafts).
func (c *Client) Releases(ctx context.Context) ([]Release, error) {
	u := fmt.Sprintf("%s/repos/%s/releases?per_page=%d", c.base(), c.repo(), releasesPerPage)
	body, err := c.get(ctx, u, "application/vnd.github+json", maxReleasesJSON)
	if errors.Is(err, errNotFound) {
		return nil, c.notFound()
	}
	if err != nil {
		return nil, err
	}
	var rs []Release
	if err := json.Unmarshal(body, &rs); err != nil {
		return nil, fmt.Errorf("decoding the release list: %w", err)
	}
	return rs, nil
}

// ReleaseByTag fetches the release tagged tag. GitHub answers 404 both for
// a missing tag and for a private repository the caller cannot see, so the
// message mentions both.
func (c *Client) ReleaseByTag(ctx context.Context, tag string) (Release, error) {
	u := fmt.Sprintf("%s/repos/%s/releases/tags/%s", c.base(), c.repo(), url.PathEscape(tag))
	body, err := c.get(ctx, u, "application/vnd.github+json", maxReleasesJSON)
	if errors.Is(err, errNotFound) {
		return Release{}, fmt.Errorf("no published release %s, or %w", tag, c.notFound())
	}
	if err != nil {
		return Release{}, err
	}
	var r Release
	if err := json.Unmarshal(body, &r); err != nil {
		return Release{}, fmt.Errorf("decoding release %s: %w", tag, err)
	}
	return r, nil
}

// notFound explains a 404, which GitHub also returns for a private
// repository the caller cannot see.
func (c *Client) notFound() error {
	if c.Token == "" {
		return fmt.Errorf("GitHub returned 404 for %s: the repository is private, so the request needs a token. "+
			"Set GH_TOKEN (or GITHUB_TOKEN) to a token that can read it, or install the GitHub CLI and run `gh auth login`, then retry", c.repo())
	}
	return fmt.Errorf("GitHub returned 404 for %s: the repository is private and the token in use cannot read it. "+
		"Use a token with read access to %s (GH_TOKEN, GITHUB_TOKEN, or `gh auth login`)", c.repo(), c.repo())
}

// ArchiveName is the release archive for a version and platform, as
// .goreleaser.yaml names it.
func ArchiveName(tag, goos, goarch string) string {
	return fmt.Sprintf("throt_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// Download fetches the archive for goos/goarch from rel and verifies its
// SHA-256 against the release's checksums.txt. It returns the archive only
// when the checksum matches.
func (c *Client) Download(ctx context.Context, rel Release, goos, goarch string) ([]byte, error) {
	name := ArchiveName(rel.TagName, goos, goarch)
	archive, sums := findAsset(rel, name), findAsset(rel, "checksums.txt")
	if archive == nil {
		return nil, fmt.Errorf("release %s has no %s (no build for %s/%s)", rel.TagName, name, goos, goarch)
	}
	if sums == nil {
		return nil, fmt.Errorf("release %s has no checksums.txt; refusing to install an unverified archive", rel.TagName)
	}
	if archive.Size > MaxArchive {
		return nil, fmt.Errorf("%s is %d bytes, more than the %d allowed", name, archive.Size, MaxArchive)
	}
	sumData, err := c.asset(ctx, sums, maxChecksums)
	if err != nil {
		return nil, err
	}
	want, err := checksumFor(sumData, name)
	if err != nil {
		return nil, err
	}
	data, err := c.asset(ctx, archive, MaxArchive)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(data)
	if !bytes.Equal(got[:], want) {
		return nil, fmt.Errorf("SHA-256 of %s is %x but checksums.txt says %x; refusing to install", name, got, want)
	}
	return data, nil
}

func (c *Client) asset(ctx context.Context, a *Asset, limit int64) ([]byte, error) {
	if a.URL == "" {
		return nil, fmt.Errorf("asset %s has no download URL", a.Name)
	}
	body, err := c.get(ctx, a.URL, "application/octet-stream", limit)
	if errors.Is(err, errNotFound) {
		return nil, fmt.Errorf("downloading %s: %w", a.Name, c.notFound())
	}
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", a.Name, err)
	}
	return body, nil
}

func findAsset(rel Release, name string) *Asset {
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			return &rel.Assets[i]
		}
	}
	return nil
}

// checksumFor finds name's SHA-256 in a sha256sum-style checksums file.
func checksumFor(data []byte, name string) ([]byte, error) {
	var found []byte
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum, err := hex.DecodeString(fields[0])
		if err != nil || len(sum) != sha256.Size {
			return nil, fmt.Errorf("checksums.txt has a malformed entry for %s", name)
		}
		if found != nil && !bytes.Equal(found, sum) {
			return nil, fmt.Errorf("checksums.txt has conflicting entries for %s", name)
		}
		found = sum
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading checksums.txt: %w", err)
	}
	if found == nil {
		return nil, fmt.Errorf("checksums.txt has no entry for %s; refusing to install an unverified archive", name)
	}
	return found, nil
}

// Token returns a GitHub token from GH_TOKEN or GITHUB_TOKEN, else from
// `gh auth token` when gh is on PATH, else "" (anonymous). It says where the
// token came from, never the token itself.
func Token(ctx context.Context, getenv func(string) string, lookPath func(string) (string, error), run Runner) (token, source string) {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if t := strings.TrimSpace(getenv(k)); validToken(t) {
			return t, k
		}
	}
	if lookPath == nil || run == nil {
		return "", ""
	}
	gh, err := lookPath("gh")
	if err != nil {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	out, err := run(ctx, gh, "auth", "token")
	if err != nil {
		return "", ""
	}
	if t := strings.TrimSpace(string(out)); validToken(t) {
		return t, "gh auth token"
	}
	return "", ""
}

// validToken rejects empty values and anything that could not be a header
// value, such as a multi-line command output.
func validToken(t string) bool {
	if t == "" || len(t) > 1024 {
		return false
	}
	for _, r := range t {
		if r > unicode.MaxASCII || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}
