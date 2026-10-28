package update

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestReleaseConfig ties the updater to the release pipeline: the archive
// and checksum names it looks for are the ones .goreleaser.yaml produces, and
// the release workflow builds with one exact GoReleaser version, so a tag
// rebuilt later yields the same archives.
func TestReleaseConfig(t *testing.T) {
	gr, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(gr)
	for _, want := range []string{
		"project_name: throt\n",
		"{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}\n",
		"formats: [tar.gz]\n",
		"name_template: checksums.txt\n",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf(".goreleaser.yaml lacks %q, which ArchiveName and Download rely on", strings.TrimSpace(want))
		}
	}
	if got := ArchiveName("v0.9.0-rc.1", "linux", "arm64"); got != "throt_0.9.0-rc.1_linux_arm64.tar.gz" {
		t.Errorf("ArchiveName = %q", got)
	}

	wf, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s+version:\s*(.+?)\s*$`).FindAllStringSubmatch(string(wf), -1)
	if len(m) != 1 {
		t.Fatalf("release.yml: want one GoReleaser version: line, found %d", len(m))
	}
	if !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(strings.Trim(m[0][1], `"'`)) {
		t.Errorf("release.yml pins GoReleaser to %s; use an exact version like v2.19.0", m[0][1])
	}
}
