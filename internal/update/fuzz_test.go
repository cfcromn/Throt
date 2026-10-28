package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// FuzzExtract checks that Extract never panics and accepts an archive only
// with exactly throt and throtd, each within maxBinary.
func FuzzExtract(f *testing.F) {
	ok := []entry{{name: "throt", body: fakeBinary("throt", "1.0.0")}, {name: "throtd", body: fakeBinary("throtd", "1.0.0")}}
	f.Add(releaseArchive(f, "1.0.0"))
	f.Add(makeArchive(f, append(ok, entry{name: "../evil", body: []byte("x")})))
	f.Add(makeArchive(f, append(ok, entry{name: "link", typ: tar.TypeSymlink, linkname: "/etc"})))
	f.Add(makeArchive(f, append(ok, entry{name: "dir/throt", body: []byte("again")})))
	f.Add(makeArchive(f, []entry{{name: "d/", typ: tar.TypeDir}, ok[0], ok[1]}))
	var plain bytes.Buffer
	zw := gzip.NewWriter(&plain)
	zw.Write([]byte("not a tar"))
	zw.Close()
	f.Add(plain.Bytes())
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, archive []byte) {
		bins, err := Extract(archive)
		if err != nil {
			return
		}
		if len(bins) != len(Binaries) {
			t.Fatalf("Extract returned %d files", len(bins))
		}
		for _, b := range Binaries {
			if data, ok := bins[b]; !ok || len(data) == 0 || int64(len(data)) > maxBinary {
				t.Fatalf("Extract: %s is %d bytes (present %v)", b, len(data), ok)
			}
		}
	})
}

// FuzzChecksumFor checks that a sum is returned only when some line is
// exactly "<64 hex digits> <name>" (or "*<name>") with that sum, and that
// two different sums for name are never resolved to one.
func FuzzChecksumFor(f *testing.F) {
	name := ArchiveName("v1.0.0", "darwin", "arm64")
	sum := sha256.Sum256([]byte("archive"))
	other := sha256.Sum256([]byte("other"))
	f.Add(fmt.Appendf(nil, "%x  %s\n%x  other.tar.gz\n", sum, name, other), name)
	f.Add(fmt.Appendf(nil, "%x *%s\r\n", sum, name), name)
	f.Add(fmt.Appendf(nil, "%x  %s\n%x  %s\n", sum, name, other, name), name)
	f.Add(fmt.Appendf(nil, "%x  %s extra\n", sum, name), name)
	f.Add(fmt.Appendf(nil, "zz  %s\n", name), name)
	f.Add([]byte(strings.Repeat("a", 70000)+"\n"), name)
	f.Fuzz(func(t *testing.T, data []byte, name string) {
		got, err := checksumFor(data, name)
		if err != nil {
			return
		}
		if len(got) != sha256.Size {
			t.Fatalf("checksumFor returned %d bytes", len(got))
		}
		matched := false
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
				continue
			}
			s, err := hex.DecodeString(fields[0])
			if err != nil || !bytes.Equal(s, got) {
				t.Fatalf("checksumFor(%q, %q) = %x despite entry %q", data, name, got, line)
			}
			matched = true
		}
		if !matched {
			t.Fatalf("checksumFor(%q, %q) = %x with no entry", data, name, got)
		}
	})
}
