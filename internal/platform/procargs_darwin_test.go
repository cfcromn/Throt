//go:build darwin

package platform

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestProcCommLayout reads real kern.procargs2 buffers: a normal argv[0],
// one whose executable path fills the alignment exactly, and an empty
// argv[0], where ps would print the first environment variable.
func TestProcCommLayout(t *testing.T) {
	for _, tc := range []struct {
		path string
		args []string
		want string
	}{
		{"/bin/cat", []string{"cat-name"}, "cat-name"},
		{"/bin/cat", []string{""}, "/bin/cat"},
		{"/bin/sleep", []string{"sleep-name", "30"}, "sleep-name"},
		{"/bin/sleep", []string{"", "30"}, "/bin/sleep"},
	} {
		cmd := &exec.Cmd{Path: tc.path, Args: tc.args, Env: []string{"SECRET_TOKEN=hunter2"}}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// Until the child has exec'd, the buffer is still the test's own.
		var got string
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if got, err = procComm(cmd.Process.Pid); err == nil && got == tc.want {
				break
			}
		}
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
		if err != nil || got != tc.want || strings.Contains(got, "SECRET") {
			t.Errorf("%s %q: procComm = %q, %v; want %q", tc.path, tc.args, got, err, tc.want)
		}
	}
}
