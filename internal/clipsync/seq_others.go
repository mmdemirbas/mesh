//go:build !windows && !(darwin && cgo)

package clipsync

// getOSClipSeq returns 0 on Linux, OR on macOS if cross-compiling with CGO disabled.
// This forces the polling loop to bypass the sequence optimization gracefully.
func getOSClipSeq() uint32 {
	return 0
}
