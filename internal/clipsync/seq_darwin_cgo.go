//go:build darwin && cgo

package clipsync

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework AppKit
#import <AppKit/AppKit.h>

long get_pasteboard_change_count() {
    // @autoreleasepool prevents memory leaks when interacting with Cocoa APIs
    @autoreleasepool {
        return [[NSPasteboard generalPasteboard] changeCount];
    }
}
*/
import "C"

// getOSClipSeq returns the macOS clipboard sequence number.
// It increments every time the clipboard contents change.
func getOSClipSeq() uint32 {
	return uint32(C.get_pasteboard_change_count())
}
