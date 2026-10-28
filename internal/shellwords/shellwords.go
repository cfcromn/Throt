// Package shellwords splits a deliberately small subset of POSIX shell
// syntax into words. It is conservative: it accepts only plain words,
// single and double quotes, backslash escapes, parameter references such as
// $HOME, and the && operator between simple commands. Anything else (pipes,
// ;, ||, &, redirections, subshells, command substitution, heredocs,
// comments, newlines, braces, !, ANSI-C quoting) makes Split refuse the
// whole input, so a caller never reasons about syntax it does not model.
// It makes no OS calls and keeps no state.
package shellwords

import "strings"

// Word is one shell word of a simple command.
type Word struct {
	// Value is the word after quote removal and escape processing.
	// Parameter references are kept literally (unexpanded).
	Value string
	// Raw is the word exactly as written in the source.
	Raw string
	// Start is the byte offset of Raw in the source.
	Start int
}

// Split parses s as one or more simple commands joined by &&. It returns
// the words of each command, in order, and ok false when s uses any
// construct outside the supported subset, has an empty command, or has an
// unterminated quote.
func Split(s string) (cmds [][]Word, ok bool) {
	var (
		cur   []Word
		val   strings.Builder
		start = -1 // start of the current word, or -1 between words
	)
	endWord := func(end int) {
		if start >= 0 {
			cur = append(cur, Word{Value: val.String(), Raw: s[start:end], Start: start})
			val.Reset()
			start = -1
		}
	}
	begin := func(i int) {
		if start < 0 {
			start = i
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t':
			endWord(i)
		case '&':
			if i+1 >= len(s) || s[i+1] != '&' {
				return nil, false // background or &>
			}
			endWord(i)
			if len(cur) == 0 {
				return nil, false
			}
			cmds = append(cmds, cur)
			cur = nil
			i++
		case '\\':
			if i+1 >= len(s) || s[i+1] == '\n' || s[i+1] == '\r' {
				return nil, false // trailing backslash or line continuation
			}
			begin(i)
			val.WriteByte(s[i+1])
			i++
		case '\'':
			begin(i)
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			lit := s[i+1 : i+1+j]
			if strings.ContainsAny(lit, "\n\r") {
				return nil, false
			}
			val.WriteString(lit)
			i += j + 1
		case '"':
			begin(i)
			end, ok := doubleQuoted(s, i+1, &val)
			if !ok {
				return nil, false
			}
			i = end
		case '$':
			if i+1 < len(s) && strings.IndexByte("('\"{", s[i+1]) >= 0 {
				return nil, false // $(...), $'...', $"...", ${...}
			}
			begin(i)
			val.WriteByte(c)
		case '|', ';', '<', '>', '(', ')', '`', '#', '!', '{', '}', '\n', '\r':
			return nil, false
		default:
			begin(i)
			val.WriteByte(c)
		}
	}
	endWord(len(s))
	if len(cur) == 0 {
		return nil, false // empty input or a trailing &&
	}
	return append(cmds, cur), true
}

// doubleQuoted consumes a double-quoted string whose content starts at i,
// appending its value to val. It returns the index of the closing quote.
// Command substitution and newlines inside the quotes are refused.
func doubleQuoted(s string, i int, val *strings.Builder) (int, bool) {
	for ; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			return i, true
		case '\\':
			if i+1 >= len(s) {
				return 0, false
			}
			n := s[i+1]
			switch n {
			case '"', '\\', '$', '`':
				val.WriteByte(n)
			case '\n', '\r':
				return 0, false
			default:
				// Inside double quotes a backslash before any other
				// character is kept.
				val.WriteByte('\\')
				val.WriteByte(n)
			}
			i++
		case '$':
			if i+1 < len(s) && s[i+1] == '(' {
				return 0, false
			}
			val.WriteByte(c)
		case '`', '\n', '\r':
			return 0, false
		default:
			val.WriteByte(c)
		}
	}
	return 0, false
}
