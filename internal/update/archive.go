package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// Variables so tests can lower them.
var (
	// maxBinary caps each extracted binary.
	maxBinary int64 = 100 << 20
	// maxUnpacked caps everything read from the decompressed archive, so a
	// compression bomb is cut off.
	maxUnpacked int64 = 256 << 20
)

// Binaries are the two programs a release archive must contain.
var Binaries = []string{"throtd", "throt"}

// errTooLarge is returned once the decompressed stream passes maxUnpacked.
var errTooLarge = errors.New("archive unpacks to more than the allowed size")

type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// Extract returns the contents of throt and throtd from a release tar.gz.
// It reads nothing to disk. The whole archive is refused when any entry is
// an absolute path or climbs with "..", or is anything but a regular file or
// a directory (symlinks, hard links, devices), when a binary is missing,
// duplicated or larger than maxBinary, or when it unpacks to more than
// maxUnpacked.
func Extract(archive []byte) (map[string][]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("reading archive: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(&capReader{r: zr, left: maxUnpacked})
	want := map[string]bool{}
	for _, b := range Binaries {
		want[b] = true
	}
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive: %w", err)
		}
		if err := checkEntryName(h.Name); err != nil {
			return nil, err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return nil, fmt.Errorf("archive entry %q is not a regular file (type %q); refusing it", h.Name, h.Typeflag)
		}
		base := path.Base(path.Clean(h.Name))
		if !want[base] {
			continue
		}
		if _, dup := out[base]; dup {
			return nil, fmt.Errorf("archive has more than one %s", base)
		}
		if h.Size < 0 || h.Size > maxBinary {
			return nil, fmt.Errorf("archive entry %s is %d bytes, more than the %d allowed", h.Name, h.Size, maxBinary)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxBinary+1))
		if err != nil {
			return nil, fmt.Errorf("reading %s from the archive: %w", h.Name, err)
		}
		if int64(len(data)) != h.Size {
			return nil, fmt.Errorf("archive entry %s is truncated", h.Name)
		}
		out[base] = data
	}
	for _, b := range Binaries {
		if out[b] == nil {
			return nil, fmt.Errorf("archive has no %s", b)
		}
	}
	return out, nil
}

// checkEntryName refuses absolute names and any ".." element.
func checkEntryName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || strings.ContainsRune(name, 0) {
		return fmt.Errorf("archive entry %q has an unsafe name; refusing it", name)
	}
	for _, el := range strings.Split(name, "/") {
		if el == ".." {
			return fmt.Errorf("archive entry %q climbs out of the archive; refusing it", name)
		}
	}
	return nil
}
