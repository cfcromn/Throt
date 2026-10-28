package update

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNetworkErrors checks that each way the API can fail reaches the user
// as an error that says what to do, from both Releases and ReleaseByTag.
func TestNetworkErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"unauthorized", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
			"GitHub rejected the token (401)"},
		{"forbidden", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) },
			"GitHub refused the request (403 Forbidden); anonymous requests are rate limited"},
		{"rate limited", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
			"GitHub refused the request (429 Too Many Requests)"},
		{"server error", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) },
			"GitHub returned 502 Bad Gateway for /repos/cfcromn/Throt/releases"},
		{"not JSON", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html>") },
			"decoding"},
		{"declared too large", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(maxReleasesJSON+1))
			w.WriteHeader(http.StatusOK)
		}, fmt.Sprintf("is %d bytes, more than the %d allowed", maxReleasesJSON+1, maxReleasesJSON)},
		{"streamed too large", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Transfer-Encoding", "chunked")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			w.Write(make([]byte, maxReleasesJSON+1))
		}, fmt.Sprintf("is more than the %d bytes allowed", maxReleasesJSON)},
		{"truncated", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "[{")
		}, "unexpected EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(tt.handler)
			defer srv.Close()
			c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
			if _, err := c.Releases(context.Background()); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Releases: err = %v, want %q", err, tt.want)
			}
			want := tt.want
			if tt.name == "server error" {
				want = "GitHub returned 502 Bad Gateway for /repos/cfcromn/Throt/releases/tags/v1.0.0"
			}
			if _, err := c.ReleaseByTag(context.Background(), "v1.0.0"); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("ReleaseByTag: err = %v, want %q", err, want)
			}
		})
	}
}

// TestConnectionRefused checks a request to a host that is not there.
func TestConnectionRefused(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
	srv.Close()
	if _, err := c.Releases(context.Background()); err == nil || !strings.Contains(err.Error(), "connect") {
		t.Fatalf("err = %v, want a connection error", err)
	}
}

// TestPartialDownload checks that an archive cut off mid-transfer is never
// installed: the transfer error, or failing that the checksum, stops it.
func TestPartialDownload(t *testing.T) {
	archive := releaseArchive(t, "0.9.0")
	name := ArchiveName("v0.9.0", "linux", "amd64")
	sums := fmt.Appendf(nil, "%x  %s\n", sha256.Sum256(archive), name)
	tests := []struct {
		name  string
		serve func(w http.ResponseWriter)
		want  string
	}{
		{"connection drops", func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
			w.WriteHeader(http.StatusOK)
			w.Write(archive[:len(archive)/2])
		}, "downloading " + name + ": unexpected EOF"},
		{"short body without length", func(w http.ResponseWriter) {
			w.Header().Set("Transfer-Encoding", "chunked")
			w.WriteHeader(http.StatusOK)
			w.Write(archive[:len(archive)/2])
		}, "SHA-256 of " + name},
		{"asset gone", func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) },
			"downloading " + name + ": GitHub returned 404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/checksums.txt") {
					w.Write(sums)
					return
				}
				tt.serve(w)
			}))
			defer srv.Close()
			rel := Release{TagName: "v0.9.0", Assets: []Asset{
				{Name: name, Size: int64(len(archive)), URL: srv.URL + "/a/" + name},
				{Name: "checksums.txt", Size: int64(len(sums)), URL: srv.URL + "/a/checksums.txt"},
			}}
			c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
			data, err := c.Download(context.Background(), rel, "linux", "amd64")
			if err == nil || !strings.Contains(err.Error(), tt.want) || data != nil {
				t.Fatalf("err = %v, want %q (data %d bytes)", err, tt.want, len(data))
			}
		})
	}
	rel := Release{TagName: "v0.9.0", Assets: []Asset{{Name: name}, {Name: "checksums.txt", URL: "https://example.invalid/c"}}}
	if _, err := (&Client{}).asset(context.Background(), &rel.Assets[0], 1); err == nil || !strings.Contains(err.Error(), "no download URL") {
		t.Errorf("asset without URL: err = %v", err)
	}
}

func TestCheckRedirect(t *testing.T) {
	https := mustRequest(t, "https://objects.example.com/a")
	plain := mustRequest(t, "http://user:secret@objects.example.com/a")
	if err := checkRedirect(https, make([]*http.Request, maxRedirects-1)); err != nil {
		t.Errorf("https redirect: %v", err)
	}
	if err := checkRedirect(https, make([]*http.Request, maxRedirects)); err == nil || err.Error() != "too many redirects" {
		t.Errorf("redirect loop: err = %v", err)
	}
	err := checkRedirect(plain, nil)
	if err == nil || !strings.Contains(err.Error(), "non-HTTPS") || strings.Contains(err.Error(), "secret") {
		t.Errorf("plain redirect: err = %v, want a refusal without the password", err)
	}
}

func mustRequest(t *testing.T, s string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestReplaceRenameFails checks both rename failures: the first (throtd)
// leaves everything as it was; the second (throt) says the two binaries now
// differ. Neither leaves a temporary file behind.
func TestReplaceRenameFails(t *testing.T) {
	bins := map[string][]byte{"throt": fakeBinary("throt", "0.9.0"), "throtd": fakeBinary("throtd", "0.9.0")}
	// A non-empty directory where a binary should be cannot be renamed
	// over.
	blocker := func(t *testing.T, p string) {
		t.Helper()
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(p, "keep"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("throtd", func(t *testing.T) {
		inst := install(t, "0.8.0")
		blocker(t, inst.Throtd)
		err := Replace(context.Background(), inst, bins, "v0.9.0", fakeRun)
		if err == nil || !strings.HasPrefix(err.Error(), "replacing "+inst.Throtd) || !strings.HasSuffix(err.Error(), "nothing was changed") {
			t.Fatalf("err = %v", err)
		}
		if data, _ := os.ReadFile(inst.Throt); string(data) != string(fakeBinary("throt", "0.8.0")) {
			t.Errorf("throt = %q, want unchanged", data)
		}
		noTemps(t, filepath.Dir(inst.Throt))
	})
	t.Run("throt", func(t *testing.T) {
		inst := install(t, "0.8.0")
		blocker(t, inst.Throt)
		err := Replace(context.Background(), inst, bins, "v0.9.0", fakeRun)
		if err == nil || !strings.HasPrefix(err.Error(), "throtd was updated but replacing "+inst.Throt) || !strings.Contains(err.Error(), "different releases") {
			t.Fatalf("err = %v", err)
		}
		if data, _ := os.ReadFile(inst.Throtd); string(data) != string(fakeBinary("throtd", "0.9.0")) {
			t.Errorf("throtd = %q, want the new one", data)
		}
		noTemps(t, filepath.Dir(inst.Throt))
	})
	t.Run("missing binary", func(t *testing.T) {
		inst := install(t, "0.8.0")
		err := Replace(context.Background(), inst, map[string][]byte{"throtd": bins["throtd"]}, "v0.9.0", fakeRun)
		if err == nil || err.Error() != "no new throt to install" {
			t.Fatalf("err = %v", err)
		}
		assertVersion(t, inst, "0.8.0")
	})
}

func noTemps(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".update-") {
			t.Errorf("leftover temporary file %s", e.Name())
		}
	}
}
