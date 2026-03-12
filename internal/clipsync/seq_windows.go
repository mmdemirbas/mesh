//go:build windows

package clipsync

import "syscall"

var (
	user32                     = syscall.NewLazyDLL("user32.dll")
	getClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
)

// getOSClipSeq returns the Windows clipboard sequence number.
// It increments every time the clipboard contents change.
func getOSClipSeq() uint32 {
	ret, _, _ := getClipboardSequenceNumber.Call()
	return uint32(ret)
}
