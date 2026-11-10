//go:build darwin

package main

import "os"

// taskpolicy ships with macOS.
const taskpolicyPath = "/usr/sbin/taskpolicy"

// lowPriorityPrefix is the command prefix that starts a program under the
// utility QoS clamp, which its children inherit: it yields the CPU to
// foreground work like the background state throtd can put a running
// process in, but keeps the performance cores and unthrottled I/O when the
// machine has room (measured on an M5 Pro, six clean builds at once: 13%
// slower than unclamped, against 5.4 times for the background state, with
// the same foreground latency). Only a spawn attribute can set the clamp,
// so only a launcher can; throtd recognizes it and leaves the process
// alone. nil when taskpolicy is missing.
func lowPriorityPrefix() []string {
	if st, err := os.Stat(taskpolicyPath); err != nil || st.Mode()&0o111 == 0 {
		return nil
	}
	return []string{taskpolicyPath, "-c", "utility"}
}
