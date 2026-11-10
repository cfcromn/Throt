package usage

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxLine is the longest log line that is read. A longer line (a large
// tool result, an attached image) is skipped without being held in
// memory.
const maxLine = 4 << 20

// Source is a directory tree of one agent kind's session logs.
type Source struct {
	Agent string
	Dir   string
}

// Sources returns where the session logs of the user with home directory
// home are, given the environment: Claude Code keeps them under
// $CLAUDE_CONFIG_DIR (default ~/.claude) in projects, Codex under
// $CODEX_HOME (default ~/.codex) in sessions.
func Sources(getenv func(string) string, home string) []Source {
	dir := func(env, def, sub string) string {
		d := getenv(env)
		if d == "" {
			d = filepath.Join(home, def)
		}
		return filepath.Join(d, sub)
	}
	return []Source{
		{Agent: ClaudeCode, Dir: dir("CLAUDE_CONFIG_DIR", ".claude", "projects")},
		{Agent: Codex, Dir: dir("CODEX_HOME", ".codex", "sessions")},
	}
}

// Scan adds the entries of every .jsonl file under the sources to c. It
// only reads. A missing directory, an unreadable file and a malformed or
// overlong line are skipped; a file last modified before the collector's
// start cannot hold an entry after it and is not opened.
func Scan(ctx context.Context, sources []Source, c *Collector) error {
	r := bufio.NewReaderSize(nil, maxLine)
	for _, src := range sources {
		err := filepath.WalkDir(src.Dir, func(path string, d fs.DirEntry, err error) error {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			if info, err := d.Info(); err != nil || info.ModTime().Before(c.since) {
				return nil
			}
			var p parser
			if src.Agent == Codex {
				p = newCodexParser(path)
			} else {
				p = newClaudeParser(path)
			}
			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			defer f.Close()
			r.Reset(f)
			c.files++
			c.skipped += readLines(r, p, c)
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// readLines feeds the lines of r to p and its entries to c, and returns
// how many lines it skipped. r's buffer bounds the length of a line.
func readLines(r *bufio.Reader, p parser, c *Collector) (skipped int) {
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			skipped++
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			if err != nil {
				return skipped
			}
			continue
		}
		// A last line without a newline is still being written.
		if line = bytes.TrimSpace(line); len(line) > 0 && err == nil {
			if e, ok, perr := p.parse(line); perr != nil {
				skipped++
			} else if ok {
				c.Add(e)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				skipped++
			}
			return skipped
		}
	}
}
