//go:build !darwin

package main

// lowPriorityPrefix is nil: throtd lowers a running process on Linux
// (nice and I/O priority) without the cost the macOS background state has.
func lowPriorityPrefix() []string { return nil }
