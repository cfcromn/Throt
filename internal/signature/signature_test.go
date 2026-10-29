package signature

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFromArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string // "" means not ok
	}{
		{"python", []string{"train.py"}, "python train.py"},
		{"python", []string{"scripts/train.py", "--epochs", "3"}, "python train.py"},
		{"python", []string{"-u", "train.py"}, "python train.py"},
		{"python", []string{"-m", "pytest"}, "python pytest"},
		{"pnpm", []string{"test:e2e"}, "pnpm test:e2e"},
		{"npx", []string{"playwright", "test"}, "npx playwright"},
		{"./scripts/build.sh", nil, "build.sh"},
		{"/usr/local/bin/gradlew", []string{"test"}, "gradlew test"},
		{"node", []string{"--max-old-space-size=8192", "build.js"}, "node build.js"},
		{"make", []string{"--", "all"}, "make all"},
		// The first non-flag argument is kept only if it is plain.
		{"python", []string{"-c", "print(1)"}, "python"},
		{"env", []string{"TOKEN=secret", "python", "x.py"}, "env"},
		{"ssh", []string{"user@host"}, "ssh"},
		{"curl", []string{"https://user:tok@example.com"}, "curl"},
		{"curl", []string{"https://example.com/x/report.csv"}, "curl report.csv"},
		{"run", []string{"$HOME"}, "run"},
		{"run", []string{"a b"}, "run"},
		{"run", []string{strings.Repeat("a", MaxWord+1)}, "run"},
		{"run", []string{"."}, "run"},
		{"run", []string{"out/dir/"}, "run dir"},
		{"run", []string{"ünï"}, "run"},
		{"run", []string{""}, "run"},
		// Not a plain program name.
		{"", nil, ""},
		{"/", nil, ""},
		{"..", nil, ""},
		{"-x", nil, ""},
		{"a:b", nil, ""},
		{"$CMD", nil, ""},
		{strings.Repeat("p", MaxWord+1), nil, ""},
	}
	for _, tt := range tests {
		got, ok := FromArgs(tt.name, tt.args)
		if ok != (tt.want != "") || got != tt.want {
			t.Errorf("FromArgs(%q, %q) = %q, %v; want %q", tt.name, tt.args, got, ok, tt.want)
		}
		if ok && !Valid(got) {
			t.Errorf("FromArgs(%q, %q) = %q is not Valid", tt.name, tt.args, got)
		}
	}
}

func TestValid(t *testing.T) {
	for _, s := range []string{"python train.py", "make", "pnpm test:e2e", "build.sh"} {
		if !Valid(s) {
			t.Errorf("Valid(%q) = false", s)
		}
	}
	for _, s := range []string{"", " ", "a  b", "a b c", "a ", " a", "-a", "a -b", "a:b c", "a b=c", "a\x00", "a\nb", "a/b", strings.Repeat("a", MaxWord+1)} {
		if Valid(s) {
			t.Errorf("Valid(%q) = true", s)
		}
	}
}

// FuzzFromArgs checks that any signature FromArgs returns is Valid, short,
// printable ASCII, and made only of base names taken from its input.
func FuzzFromArgs(f *testing.F) {
	f.Add("python", "train.py", "--x")
	f.Add("./a/b.sh", "-q", "c/d:e")
	f.Add("", "", "")
	f.Add("x", "a=b", "y")
	f.Fuzz(func(t *testing.T, name, a1, a2 string) {
		s, ok := FromArgs(name, []string{a1, a2})
		if !ok {
			if s != "" {
				t.Fatalf("not ok but %q", s)
			}
			return
		}
		if !Valid(s) || len(s) > 2*MaxWord+1 || !utf8.ValidString(s) {
			t.Fatalf("FromArgs(%q, %q, %q) = %q", name, a1, a2, s)
		}
		if strings.ContainsAny(s, "/=@$\"'\\\n\t") {
			t.Fatalf("signature %q keeps more than base names", s)
		}
	})
}

func FuzzValid(f *testing.F) {
	f.Add("python train.py")
	f.Add("a b c")
	f.Fuzz(func(t *testing.T, s string) {
		if !Valid(s) {
			return
		}
		prog, arg, two := strings.Cut(s, " ")
		args := []string(nil)
		if two {
			args = []string{arg}
		}
		if got, ok := FromArgs(prog, args); !ok || got != s {
			t.Fatalf("Valid(%q) but FromArgs gives %q, %v", s, got, ok)
		}
	})
}
