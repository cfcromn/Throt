package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestCompletionCoversEveryCommand(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			var b bytes.Buffer
			if err := printCompletion(&b, shell); err != nil {
				t.Fatal(err)
			}
			words := map[string]bool{}
			for _, w := range regexp.MustCompile(`[a-z]+`).FindAllString(b.String(), -1) {
				words[w] = true
			}
			for _, c := range commands() {
				if !words[c.name] {
					t.Errorf("%s script lacks command %q", shell, c.name)
				}
			}
			for _, flag := range []string{"config", "timeout", "no-wait"} {
				if !strings.Contains(b.String(), flag) {
					t.Errorf("%s script lacks flag %q", shell, flag)
				}
			}
		})
	}
}

func TestCompletionUnknownShell(t *testing.T) {
	if err := printCompletion(&bytes.Buffer{}, "tcsh"); err == nil {
		t.Fatal("want an error")
	}
}
