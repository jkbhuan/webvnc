//go:build !windows || !amd64

// The probe drives a Windows-only component, so on any other platform the
// command exists but has nothing to do. It is kept compiling rather than
// excluded outright so that a plain `go build ./...` works everywhere.
package main

import "fmt"

func main() {
	fmt.Println("mfprobe drives the Windows Media Foundation encoder; it only runs on 64-bit Windows.")
}
