package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
)

// The fake binaries are text: "fake-<name> <version>". fakeRun plays the
// part of running one: it prints the version a fake binary holds.
func fakeBinary(name, version string) []byte {
	return []byte("fake-" + name + " " + version + "\n")
}

func fakeRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	f := strings.Fields(string(data))
	if len(f) != 2 || !strings.HasPrefix(f[0], "fake-") {
		return nil, errors.New("exec format error")
	}
	return []byte(f[1] + "\n"), nil
}

type entry struct {
	name     string
	typ      byte
	body     []byte
	linkname string
}

func makeArchive(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o755, Linkname: e.linkname}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func releaseArchive(t *testing.T, version string) []byte {
	return makeArchive(t, []entry{
		{name: "LICENSE", body: []byte("license")},
		{name: "throt", body: fakeBinary("throt", version)},
		{name: "throtd", body: fakeBinary("throtd", version)},
		{name: "README.md", body: []byte("readme")},
	})
}

// fakeGitHub serves releases, release assets (through a redirect, as
// GitHub does) and records the Authorization headers it saw.
type fakeGitHub struct {
	t        *testing.T
	srv      *httptest.Server
	releases []Release
	files    map[string][]byte // asset name -> content, per tag "tag/name"
	notFound bool

	mu    sync.Mutex
	auths []string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{t: t, files: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/cfcromn/Throt/releases", func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		if g.notFound {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, g.list())
	})
	mux.HandleFunc("/repos/cfcromn/Throt/releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		for _, rel := range g.list() {
			if rel.TagName == r.PathValue("tag") && !g.notFound {
				writeJSON(w, rel)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/cfcromn/Throt/releases/assets/{tag}/{name}", func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		if r.Header.Get("Accept") != "application/octet-stream" {
			http.Error(w, "want octet-stream", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/download/"+r.PathValue("tag")+"/"+r.PathValue("name"), http.StatusFound)
	})
	mux.HandleFunc("/download/{tag}/{name}", func(w http.ResponseWriter, r *http.Request) {
		data, ok := g.files[r.PathValue("tag")+"/"+r.PathValue("name")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	})
	g.srv = httptest.NewTLSServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) record(r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.auths = append(g.auths, r.Header.Get("Authorization"))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// list fills in each release's asset URLs, which point at the server.
func (g *fakeGitHub) list() []Release {
	out := make([]Release, len(g.releases))
	for i, r := range g.releases {
		r.Assets = nil
		for key, data := range g.files {
			tag, name, _ := strings.Cut(key, "/")
			if tag == r.TagName {
				r.Assets = append(r.Assets, Asset{Name: name, Size: int64(len(data)), URL: g.srv.URL + "/repos/cfcromn/Throt/releases/assets/" + key})
			}
		}
		out[i] = r
	}
	return out
}

// publish adds a release with a real archive for goos/goarch and a matching
// checksums.txt.
func (g *fakeGitHub) publish(tag string, pre bool, archive []byte) {
	g.releases = append(g.releases, Release{TagName: tag, Prerelease: pre, HTMLURL: "https://github.com/cfcromn/Throt/releases/tag/" + tag})
	if archive == nil {
		return
	}
	name := ArchiveName(tag, "linux", "amd64")
	g.files[tag+"/"+name] = archive
	g.files[tag+"/checksums.txt"] = fmt.Appendf(nil, "%x  %s\n%x  other.tar.gz\n", sha256.Sum256(archive), name, sha256.Sum256([]byte("x")))
}

// install puts fake throt and throtd of version in a new directory.
func install(t *testing.T, version string) Installed {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := Installed{Throt: filepath.Join(dir, "throt"), Throtd: filepath.Join(dir, "throtd")}
	for name, p := range map[string]string{"throt": inst.Throt, "throtd": inst.Throtd} {
		if err := os.WriteFile(p, fakeBinary(name, version), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return inst
}

func (g *fakeGitHub) env(inst Installed, current string, out io.Writer) Env {
	return Env{
		HTTP:       g.srv.Client(),
		BaseURL:    g.srv.URL,
		Repo:       DefaultRepo,
		Executable: inst.Throt,
		Current:    current,
		GOOS:       "linux",
		GOARCH:     "amd64",
		Getenv:     func(string) string { return "" },
		LookPath:   func(string) (string, error) { return "", errors.New("not found") },
		Run:        fakeRun,
		Stdin:      strings.NewReader(""),
		Stdout:     out,
	}
}

// assertVersion checks both installed binaries hold version and that no
// temporary file was left behind.
func assertVersion(t *testing.T, inst Installed, version string) {
	t.Helper()
	for name, p := range map[string]string{"throt": inst.Throt, "throtd": inst.Throtd} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if want := fakeBinary(name, version); !bytes.Equal(data, want) {
			t.Errorf("%s = %q, want %q", name, data, want)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("%s mode %v, want 0755", name, fi.Mode())
		}
	}
	ents, err := os.ReadDir(filepath.Dir(inst.Throt))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover temporary file %s", e.Name())
		}
	}
}

func TestCompareVersions(t *testing.T) {
	order := []string{"0.1.0-rc.1", "0.1.0-rc.2", "0.1.0-rc.10", "0.1.0", "v0.2.0-alpha", "0.2.0-alpha.1", "0.2.0-beta", "0.2.0", "0.10.0", "1.0.0"}
	for i := range order {
		for j := range order {
			a, ok1 := parseVersion(order[i])
			b, ok2 := parseVersion(order[j])
			if !ok1 || !ok2 {
				t.Fatalf("parse %q %q", order[i], order[j])
			}
			if got, want := compare(a, b), cmpInt(i, j); got != want {
				t.Errorf("compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
	for _, bad := range []string{"", "dev", "dev-abc1234", "1.2", "1.2.3.4", "01.2.3", "1.2.3-", "1.2.3-a..b", "v1.x.3"} {
		if _, ok := parseVersion(bad); ok {
			t.Errorf("parseVersion(%q) accepted", bad)
		}
	}
	if v, ok := parseVersion("v1.2.3-rc.1+dirty"); !ok || len(v.pre) != 2 {
		t.Errorf("build metadata: %+v %v", v, ok)
	}
}

func TestSelect(t *testing.T) {
	rs := []Release{
		{TagName: "v0.9.0-rc.1", Prerelease: true},
		{TagName: "v0.8.1"},
		{TagName: "v1.0.0", Draft: true},
		{TagName: "v0.8.0"},
		{TagName: "nightly"},
		{TagName: "v0.7.0"},
	}
	tests := []struct {
		name, current, want string
		pre                 bool
		tag                 string
		cmp                 int
		upToDate            bool
		err                 string
	}{
		{name: "stable", current: "0.8.0", tag: "v0.8.1", cmp: 1},
		{name: "stable skips prerelease and draft", current: "0.7.0", tag: "v0.8.1", cmp: 1},
		{name: "pre flag", current: "0.8.0", pre: true, tag: "v0.9.0-rc.1", cmp: 1},
		{name: "prerelease current implies pre", current: "0.8.0-rc.1", tag: "v0.9.0-rc.1", cmp: 1},
		{name: "up to date", current: "0.8.1", tag: "v0.8.1", upToDate: true},
		{name: "up to date with v prefix", current: "v0.8.1", tag: "v0.8.1", upToDate: true},
		{name: "newer than any release", current: "0.9.0", tag: "v0.8.1", cmp: -1, upToDate: true},
		{name: "explicit upgrade", current: "0.7.0", want: "0.8.0", tag: "v0.8.0", cmp: 1},
		{name: "explicit downgrade", current: "0.8.1", want: "v0.7.0", tag: "v0.7.0", cmp: -1},
		{name: "explicit same", current: "0.8.0", want: "v0.8.0", tag: "v0.8.0", upToDate: true},
		{name: "explicit draft", current: "0.8.0", want: "v1.0.0", err: "no published release"},
		{name: "explicit missing", current: "0.8.0", want: "v3.0.0", err: "no published release"},
		{name: "bad explicit", current: "0.8.0", want: "latest", err: "not a version"},
		{name: "dev current", current: "dev", err: "not a release version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Select(rs, tt.current, tt.want, tt.pre)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Release.TagName != tt.tag || c.Cmp != tt.cmp || c.UpToDate() != tt.upToDate {
				t.Errorf("got %s cmp %d upToDate %v, want %s cmp %d upToDate %v", c.Release.TagName, c.Cmp, c.UpToDate(), tt.tag, tt.cmp, tt.upToDate)
			}
		})
	}
	if _, err := Select([]Release{{TagName: "v1.0.0-rc.1", Prerelease: true}}, "0.9.0", "", false); err == nil || !strings.Contains(err.Error(), "--pre") {
		t.Errorf("only prereleases: err = %v", err)
	}
}

func TestToken(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	noGH := func(string) (string, error) { return "", errors.New("not found") }
	hasGH := func(string) (string, error) { return "/usr/bin/gh", nil }
	ghSays := func(out string, err error) Runner {
		return func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name != "/usr/bin/gh" || strings.Join(args, " ") != "auth token --hostname github.com" {
				t.Errorf("ran %s %v", name, args)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Error("gh auth token runs without a timeout")
			}
			return []byte(out), err
		}
	}
	tests := []struct {
		name     string
		getenv   map[string]string
		lookPath func(string) (string, error)
		run      Runner
		token    string
		source   string
	}{
		{"GH_TOKEN first", map[string]string{"GH_TOKEN": "a", "GITHUB_TOKEN": "b"}, hasGH, ghSays("c", nil), "a", "GH_TOKEN"},
		{"GITHUB_TOKEN", map[string]string{"GITHUB_TOKEN": "b"}, hasGH, ghSays("c", nil), "b", "GITHUB_TOKEN"},
		{"gh", nil, hasGH, ghSays("gho_xyz\n", nil), "gho_xyz", "gh auth token"},
		{"gh fails", nil, hasGH, ghSays("", errors.New("not logged in")), "", ""},
		{"gh garbage", nil, hasGH, ghSays("two\nlines", nil), "", ""},
		{"no gh", nil, noGH, nil, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok, src := Token(context.Background(), env(tt.getenv), tt.lookPath, tt.run)
			if tok != tt.token || src != tt.source {
				t.Errorf("got %q from %q, want %q from %q", tok, src, tt.token, tt.source)
			}
		})
	}
}

func TestAuthHeader(t *testing.T) {
	for _, token := range []string{"", "secret-token"} {
		g := newFakeGitHub(t)
		g.publish("v0.9.0", false, releaseArchive(t, "0.9.0"))
		c := &Client{HTTP: g.srv.Client(), BaseURL: g.srv.URL, Token: token}
		rs, err := c.Releases(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Download(context.Background(), rs[0], "linux", "amd64"); err != nil {
			t.Fatal(err)
		}
		want := ""
		if token != "" {
			want = "Bearer " + token
		}
		if len(g.auths) != 3 {
			t.Fatalf("requests %d, want 3 (list, checksums, archive)", len(g.auths))
		}
		for _, got := range g.auths {
			if got != want {
				t.Errorf("token %q: Authorization %q, want %q", token, got, want)
			}
		}
	}
}

// An asset URL on another host than the API gets no token, even though it
// comes from the release JSON rather than from a redirect.
func TestTokenStaysOnAPIHost(t *testing.T) {
	api := newFakeGitHub(t)
	archive := releaseArchive(t, "0.9.0")
	name := ArchiveName("v0.9.0", "linux", "amd64")
	sums := fmt.Appendf(nil, "%x  %s\n", sha256.Sum256(archive), name)
	var mu sync.Mutex
	var auths []string
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/checksums.txt") {
			w.Write(sums)
			return
		}
		w.Write(archive)
	}))
	t.Cleanup(other.Close)
	rel := Release{TagName: "v0.9.0", Assets: []Asset{
		{Name: name, Size: int64(len(archive)), URL: other.URL + "/a/" + name},
		{Name: "checksums.txt", Size: int64(len(sums)), URL: other.URL + "/a/checksums.txt"},
	}}
	c := &Client{HTTP: api.srv.Client(), BaseURL: api.srv.URL, Token: "secret-token"}
	if _, err := c.Download(context.Background(), rel, "linux", "amd64"); err != nil {
		t.Fatal(err)
	}
	if len(auths) != 2 {
		t.Fatalf("requests %d, want 2", len(auths))
	}
	for _, got := range auths {
		if got != "" {
			t.Errorf("asset host got Authorization %q", got)
		}
	}
}

func TestPrivateRepoNotFound(t *testing.T) {
	for _, token := range []string{"", "secret-token"} {
		g := newFakeGitHub(t)
		g.notFound = true
		c := &Client{HTTP: g.srv.Client(), BaseURL: g.srv.URL, Token: token}
		_, err := c.Releases(context.Background())
		if err == nil {
			t.Fatal("want an error")
		}
		msg := err.Error()
		for _, want := range []string{"404", "private", "GH_TOKEN", "gh auth login"} {
			if !strings.Contains(msg, want) {
				t.Errorf("token %q: %q lacks %q", token, msg, want)
			}
		}
		if token != "" && strings.Contains(msg, token) {
			t.Errorf("error leaks the token: %q", msg)
		}
	}
}

func TestRefusesPlainHTTP(t *testing.T) {
	c := &Client{BaseURL: "http://api.github.com"}
	if _, err := c.Releases(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("err = %v", err)
	}
	req, _ := http.NewRequest("GET", "http://example.com/x", nil)
	if err := checkRedirect(req, nil); err == nil {
		t.Error("redirect to http allowed")
	}
}

func TestRunUpdatesBothBinaries(t *testing.T) {
	g := newFakeGitHub(t)
	g.publish("v0.8.0", false, releaseArchive(t, "0.8.0"))
	g.publish("v0.9.0", false, releaseArchive(t, "0.9.0"))
	inst := install(t, "0.8.0")
	// Run through a symlink, as with a link in ~/bin: the file it points at
	// is replaced and the link is kept.
	link := filepath.Join(t.TempDir(), "throt")
	if err := os.Symlink(inst.Throt, link); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	e := g.env(inst, "0.8.0", &out)
	e.Executable = link
	e.Ping = func(context.Context) (string, error) { return "0.8.0", nil }
	if err := Run(context.Background(), Options{Yes: true}, e); err != nil {
		t.Fatal(err)
	}
	assertVersion(t, inst, "0.9.0")
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink replaced: %v %v", fi, err)
	}
	for _, want := range []string{"verified", "updated", "throtd is still running 0.8.0", "stopping throtd resumes everything it paused"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	// A rollback by --version installs the older release.
	out.Reset()
	e.Current = "0.9.0"
	if err := Run(context.Background(), Options{Yes: true, Version: "v0.8.0"}, e); err != nil {
		t.Fatal(err)
	}
	assertVersion(t, inst, "0.8.0")
}

func TestRunNoChangeOnFailure(t *testing.T) {
	good := releaseArchive(t, "0.9.0")
	tests := []struct {
		name    string
		archive []byte
		tamper  func(g *fakeGitHub)
		run     Runner
		err     string
	}{
		{name: "checksum mismatch", archive: good, tamper: func(g *fakeGitHub) {
			g.files["v0.9.0/"+ArchiveName("v0.9.0", "linux", "amd64")] = releaseArchive(t, "6.6.6")
		}, err: "SHA-256"},
		{name: "no checksum entry", archive: good, tamper: func(g *fakeGitHub) {
			g.files["v0.9.0/checksums.txt"] = []byte("0000  other\n")
		}, err: "no entry"},
		{name: "no checksums.txt", archive: good, tamper: func(g *fakeGitHub) {
			delete(g.files, "v0.9.0/checksums.txt")
		}, err: "no checksums.txt"},
		{name: "no archive for platform", archive: good, tamper: func(g *fakeGitHub) {
			delete(g.files, "v0.9.0/"+ArchiveName("v0.9.0", "linux", "amd64"))
		}, err: "no build for linux/amd64"},
		{name: "new binary reports another version", archive: makeArchive(t, []entry{
			{name: "throt", body: fakeBinary("throt", "0.9.0")},
			{name: "throtd", body: fakeBinary("throtd", "0.8.5")},
		}), err: "failed its check"},
		{name: "new binary does not run", archive: good, run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, errors.New("exec format error")
		}, err: "failed its check"},
		{name: "malicious archive", archive: makeArchive(t, []entry{
			{name: "throt", body: fakeBinary("throt", "0.9.0")},
			{name: "throtd", typ: tar.TypeSymlink, linkname: "/bin/sh"},
		}), err: "not a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newFakeGitHub(t)
			g.publish("v0.9.0", false, tt.archive)
			if tt.tamper != nil {
				tt.tamper(g)
			}
			inst := install(t, "0.8.0")
			e := g.env(inst, "0.8.0", io.Discard)
			if tt.run != nil {
				e.Run = tt.run
			}
			err := Run(context.Background(), Options{Yes: true}, e)
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("err = %v, want %q", err, tt.err)
			}
			assertVersion(t, inst, "0.8.0")
		})
	}
}

func TestUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	g := newFakeGitHub(t)
	g.publish("v0.9.0", false, releaseArchive(t, "0.9.0"))
	inst := install(t, "0.8.0")
	dir := filepath.Dir(inst.Throt)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	err := Run(context.Background(), Options{Yes: true}, g.env(inst, "0.8.0", io.Discard))
	var nw *NotWritableError
	if !errors.As(err, &nw) || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v, want NotWritableError", err)
	}
	assertVersion(t, inst, "0.8.0")
}

func TestConfirmation(t *testing.T) {
	g := newFakeGitHub(t)
	g.publish("v0.9.0", false, releaseArchive(t, "0.9.0"))

	inst := install(t, "0.8.0")
	err := Run(context.Background(), Options{}, g.env(inst, "0.8.0", io.Discard))
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("non-TTY without --yes: err = %v", err)
	}
	assertVersion(t, inst, "0.8.0")

	var out bytes.Buffer
	e := g.env(inst, "0.8.0", &out)
	e.IsTTY, e.Stdin = true, strings.NewReader("n\n")
	if err := Run(context.Background(), Options{}, e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[y/N]") || !strings.Contains(out.String(), "not updated") {
		t.Errorf("output %q", out.String())
	}
	assertVersion(t, inst, "0.8.0")

	e.Stdin = strings.NewReader("yes\n")
	if err := Run(context.Background(), Options{}, e); err != nil {
		t.Fatal(err)
	}
	assertVersion(t, inst, "0.9.0")
}

func TestCheck(t *testing.T) {
	g := newFakeGitHub(t)
	g.publish("v0.8.0", false, releaseArchive(t, "0.8.0"))
	g.publish("v0.9.0-rc.1", true, nil)
	inst := install(t, "0.8.0")
	tests := []struct {
		current string
		o       Options
		want    error
		out     string
	}{
		{"0.8.0", Options{Check: true}, nil, "up to date"},
		{"0.7.0", Options{Check: true}, ErrUpdateAvailable, "latest    0.8.0"},
		{"0.8.0", Options{Check: true, Pre: true}, ErrUpdateAvailable, "releases/tag/v0.9.0-rc.1"},
		{"0.8.0-rc.1", Options{Check: true}, ErrUpdateAvailable, "latest    0.9.0-rc.1"},
		{"0.8.0", Options{Check: true, Version: "0.8.0"}, nil, "requested 0.8.0"},
	}
	for _, tt := range tests {
		var out bytes.Buffer
		err := Run(context.Background(), tt.o, g.env(inst, tt.current, &out))
		if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
			t.Errorf("%s %+v: err = %v, want %v", tt.current, tt.o, err, tt.want)
		}
		if !strings.Contains(out.String(), tt.out) || !strings.Contains(out.String(), "current   "+tt.current) {
			t.Errorf("%s %+v: output %q lacks %q", tt.current, tt.o, out.String(), tt.out)
		}
	}
	assertVersion(t, inst, "0.8.0")
	if ExitUpdateAvailable != 10 {
		t.Errorf("ExitUpdateAvailable = %d, documented as 10", ExitUpdateAvailable)
	}
}

func TestLocate(t *testing.T) {
	inst := install(t, "0.8.0")
	dir := filepath.Dir(inst.Throt)
	module := &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v0.8.0"}}

	var gi *GoInstallError
	_, err := Locate(inst.Throt, "dev-abc1234", nil, nil, "")
	if !errors.As(err, &gi) || !strings.Contains(err.Error(), "go install github.com/cfcromn/Throt/cmd/throt@latest") ||
		!strings.Contains(err.Error(), "go install github.com/cfcromn/Throt/cmd/throtd@latest") {
		t.Errorf("dev build: %v", err)
	}
	_, err = Locate(inst.Throt, "v0.8.0", module, []string{"/elsewhere", dir}, "0.9.0")
	if !errors.As(err, &gi) || !strings.Contains(err.Error(), "cmd/throtd@v0.9.0") {
		t.Errorf("go install: %v", err)
	}
	// A module version outside GOBIN is a release binary (Go stamps the
	// VCS tag into release builds too).
	got, err := Locate(inst.Throt, "0.8.0", module, []string{"/elsewhere"}, "")
	if err != nil || got != inst {
		t.Errorf("release build: %+v %v", got, err)
	}
	if err := os.Remove(inst.Throtd); err != nil {
		t.Fatal(err)
	}
	if _, err := Locate(inst.Throt, "0.8.0", nil, nil, ""); err == nil || !strings.Contains(err.Error(), "throtd is not next to throt") {
		t.Errorf("missing throtd: %v", err)
	}
	if d := GoBinDirs(func(k string) string {
		return map[string]string{"GOPATH": "/a" + string(filepath.ListSeparator) + "/b"}[k]
	}, "/home/u"); strings.Join(d, ",") != "/a/bin,/b/bin" {
		t.Errorf("GoBinDirs = %v", d)
	}
	if d := GoBinDirs(func(k string) string { return map[string]string{"GOBIN": "/gobin"}[k] }, "/home/u"); strings.Join(d, ",") != "/gobin,/home/u/go/bin" {
		t.Errorf("GoBinDirs = %v", d)
	}
}

func TestExtractRefusesMaliciousArchives(t *testing.T) {
	ok := []entry{{name: "throt", body: fakeBinary("throt", "1.0.0")}, {name: "throtd", body: fakeBinary("throtd", "1.0.0")}}
	with := func(extra ...entry) []entry { return append(append([]entry{}, ok...), extra...) }
	tests := []struct {
		name    string
		entries []entry
		err     string
	}{
		{"traversal", with(entry{name: "../../evil", body: []byte("x")}), "climbs"},
		{"traversal binary", []entry{{name: "../throt", body: []byte("x")}, ok[1]}, "climbs"},
		{"absolute", with(entry{name: "/etc/passwd", body: []byte("x")}), "unsafe"},
		{"symlink", with(entry{name: "link", typ: tar.TypeSymlink, linkname: "/etc"}), "not a regular file"},
		{"hard link", with(entry{name: "hl", typ: tar.TypeLink, linkname: "throt"}), "not a regular file"},
		{"fifo", with(entry{name: "fifo", typ: tar.TypeFifo}), "not a regular file"},
		{"duplicate", with(entry{name: "dir/throt", body: []byte("x")}), "more than one"},
		{"missing throtd", ok[:1], "no throtd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Extract(makeArchive(t, tt.entries))
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("err = %v, want %q", err, tt.err)
			}
		})
	}
	if _, err := Extract([]byte("not gzip")); err == nil {
		t.Error("garbage accepted")
	}
	bins, err := Extract(makeArchive(t, append([]entry{{name: "throt_1.0.0/", typ: tar.TypeDir}}, ok...)))
	if err != nil || string(bins["throtd"]) != string(ok[1].body) {
		t.Errorf("good archive: %v", err)
	}

	defer func(b, u int64) { maxBinary, maxUnpacked = b, u }(maxBinary, maxUnpacked)
	maxBinary = 16
	if _, err := Extract(makeArchive(t, []entry{ok[0], {name: "throtd", body: bytes.Repeat([]byte("x"), 17)}})); err == nil || !strings.Contains(err.Error(), "allowed") {
		t.Errorf("oversize binary: %v", err)
	}
	maxBinary, maxUnpacked = 100<<20, 4096
	bomb := makeArchive(t, with(entry{name: "padding", body: make([]byte, 1<<20)}))
	if _, err := Extract(bomb); !errors.Is(err, errTooLarge) {
		t.Errorf("bomb: %v", err)
	}
}

func TestDownloadSizeCap(t *testing.T) {
	g := newFakeGitHub(t)
	g.publish("v0.9.0", false, releaseArchive(t, "0.9.0"))
	rel := g.list()[0]
	for i := range rel.Assets {
		if strings.HasSuffix(rel.Assets[i].Name, ".tar.gz") {
			rel.Assets[i].Size = MaxArchive + 1
		}
	}
	c := &Client{HTTP: g.srv.Client(), BaseURL: g.srv.URL}
	if _, err := c.Download(context.Background(), rel, "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "allowed") {
		t.Fatalf("err = %v", err)
	}
}

func TestRestartHint(t *testing.T) {
	has := func(want string) func(string) bool { return func(p string) bool { return strings.HasSuffix(p, want) } }
	if h := RestartHint("darwin", "/Users/u", has("dev.throt.throtd.plist")); h != "launchctl kickstart -k gui/$(id -u)/dev.throt.throtd" {
		t.Errorf("launchd: %q", h)
	}
	if h := RestartHint("linux", "/home/u", has("systemd/user/throtd.service")); h != "systemctl --user restart throtd" {
		t.Errorf("systemd: %q", h)
	}
	if h := RestartHint("linux", "/home/u", has("nothing")); !strings.Contains(h, "start it again") {
		t.Errorf("manual: %q", h)
	}
}
