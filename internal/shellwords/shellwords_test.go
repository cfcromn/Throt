package shellwords

import (
	"reflect"
	"testing"
)

func values(cmds [][]Word) [][]string {
	out := make([][]string, len(cmds))
	for i, c := range cmds {
		for _, w := range c {
			out[i] = append(out[i], w.Value)
		}
	}
	return out
}

func TestSplit(t *testing.T) {
	tests := []struct {
		in   string
		want [][]string // nil means refused
	}{
		{"make", [][]string{{"make"}}},
		{"  make  -j4\t", [][]string{{"make", "-j4"}}},
		{"cd x && make", [][]string{{"cd", "x"}, {"make"}}},
		{"cd x&&make", [][]string{{"cd", "x"}, {"make"}}},
		{`echo 'a b' "c d"`, [][]string{{"echo", "a b", "c d"}}},
		{`echo a\ b`, [][]string{{"echo", "a b"}}},
		{`echo "a\"b" 'c\d'`, [][]string{{"echo", `a"b`, `c\d`}}},
		{`echo "a\nb"`, [][]string{{"echo", `a\nb`}}},
		{`echo "$HOME" $PATH`, [][]string{{"echo", "$HOME", "$PATH"}}},
		{`echo "${HOME}"`, [][]string{{"echo", "${HOME}"}}},
		{`echo "a|b;c>d&e"`, [][]string{{"echo", "a|b;c>d&e"}}},
		{`echo 'a&&b'`, [][]string{{"echo", "a&&b"}}},
		{`FOO="a b" make`, [][]string{{"FOO=a b", "make"}}},
		{`echo ''`, [][]string{{"echo", ""}}},
		{`echo a""b`, [][]string{{"echo", "ab"}}},
		{`echo \|`, [][]string{{"echo", "|"}}},
		{"", nil},
		{"   ", nil},
		{"&& make", nil},
		{"make &&", nil},
		{"a && && b", nil},
		{"make | tee log", nil},
		{"make || true", nil},
		{"make; ls", nil},
		{"make &", nil},
		{"make &> log", nil},
		{"make > log", nil},
		{"make < in", nil},
		{"make 2>&1", nil},
		{"cat <<EOF", nil},
		{"(make)", nil},
		{"{ make; }", nil},
		{"make $(nproc)", nil},
		{`make "$(nproc)"`, nil},
		{"make `nproc`", nil},
		{`make "a` + "`" + `b"`, nil},
		{"make ${X}", nil},
		{"make $'a'", nil},
		{`make $"a"`, nil},
		{"make # comment", nil},
		{"! make", nil},
		{"make\nls", nil},
		{"make \\\nall", nil},
		{"make 'a\nb'", nil},
		{`make "unterminated`, nil},
		{`make 'unterminated`, nil},
		{`make trailing\`, nil},
	}
	for _, tt := range tests {
		got, ok := Split(tt.in)
		if tt.want == nil {
			if ok {
				t.Errorf("Split(%q) = %q, want refused", tt.in, values(got))
			}
			continue
		}
		if !ok || !reflect.DeepEqual(values(got), tt.want) {
			t.Errorf("Split(%q) = %q, %v; want %q", tt.in, values(got), ok, tt.want)
		}
	}
}

func TestSplitRawAndStart(t *testing.T) {
	in := `cd "my dir" && FOO='1' make`
	cmds, ok := Split(in)
	if !ok || len(cmds) != 2 {
		t.Fatalf("Split(%q) = %v, %v", in, cmds, ok)
	}
	for _, c := range cmds {
		for _, w := range c {
			if in[w.Start:w.Start+len(w.Raw)] != w.Raw {
				t.Errorf("word %+v: Raw does not match the source at Start", w)
			}
		}
	}
	if w := cmds[1][0]; w.Raw != "FOO='1'" || w.Value != "FOO=1" {
		t.Errorf("assignment word = %+v", w)
	}
	if w := cmds[1][1]; w.Raw != "make" || w.Start != len(in)-len("make") {
		t.Errorf("command word = %+v", w)
	}
}

// FuzzSplit checks that every accepted word points at its own source text
// and that words never overlap.
func FuzzSplit(f *testing.F) {
	for _, s := range []string{"make", `cd "a b" && FOO=1 make -j4`, `echo 'x' "y\"z" a\ b`, "a && b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		cmds, ok := Split(s)
		if !ok {
			return
		}
		end := 0
		for _, c := range cmds {
			if len(c) == 0 {
				t.Fatalf("Split(%q) returned an empty command", s)
			}
			for _, w := range c {
				if w.Start < end || w.Start+len(w.Raw) > len(s) || s[w.Start:w.Start+len(w.Raw)] != w.Raw {
					t.Fatalf("Split(%q): bad word %+v", s, w)
				}
				end = w.Start + len(w.Raw)
			}
		}
	})
}
