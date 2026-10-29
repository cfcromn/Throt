// Package signature reduces a command line to the short signature throtd
// learns heavy commands by: the program's base name and, when there is
// one, the base name of its first non-flag argument, e.g. "python
// train.py", "pnpm test:e2e" or "npx playwright". Nothing else of the
// command is kept: no directories, no further arguments, no environment.
//
// A signature comes from command text the agent itself handed to the
// Claude Code hook or to `throt run`; Throt never reads process arguments
// from the OS. It is still treated as sensitive: it only travels over the
// local socket and is only stored in throtd's private state directory.
package signature

import (
	"path"
	"strings"
)

// MaxWord bounds each of the two words of a signature, in bytes.
const MaxWord = 64

// FromArgs returns the signature of a program run with args, and ok false
// when name is not a plain program name. args are the words after the
// program, as the shell would pass them; the first that does not start
// with "-" is kept, reduced to its base name, if it is a plain word.
func FromArgs(name string, args []string) (string, bool) {
	prog := path.Base(name)
	if !word(prog, false) {
		return "", false
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if strings.Contains(a, "/") {
			a = path.Base(a)
		}
		if word(a, true) {
			return prog + " " + a, true
		}
		break
	}
	return prog, true
}

// Valid reports whether s is a signature FromArgs can return. throtd
// accepts nothing else from a client.
func Valid(s string) bool {
	prog, arg, two := strings.Cut(s, " ")
	return word(prog, false) && (!two || word(arg, true))
}

// word reports whether w is a plain word of a signature: 1 to MaxWord
// bytes of letters, digits and "_.+-" (and ":" in an argument, as in
// "test:e2e"), not starting with "-" and not "." or "..". Anything else,
// such as quoting, expansions, "=" (a possible secret in KEY=value), "@"
// (user@host) or a URL, does not count.
func word(w string, arg bool) bool {
	if w == "" || len(w) > MaxWord || w[0] == '-' || w == "." || w == ".." {
		return false
	}
	for i := 0; i < len(w); i++ {
		c := w[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '_' || c == '.' || c == '+' || c == '-':
		case c == ':' && arg:
		default:
			return false
		}
	}
	return true
}
