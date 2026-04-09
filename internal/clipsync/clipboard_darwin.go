//go:build darwin

package clipsync

import (
	"context"
	"fmt"
	"strings"
	"sync"

	pb "github.com/mmdemirbas/mesh/internal/clipsync/proto"
)

// darwinReadScript is cached because clipTmpDir() and clipFormatTable are both
// fixed for the lifetime of the process, making the script string invariant.
var darwinReadScript = sync.OnceValue(func() string {
	dir := clipTmpDir()
	var pairs []string
	for _, e := range clipFormatTable {
		pairs = append(pairs, fmt.Sprintf(`{"%s", "/%s"}`, e.darwinUTI, e.fileName))
	}
	return fmt.Sprintf(`use framework "AppKit"
set pb to current application's NSPasteboard's generalPasteboard()
set tmpDir to "%s"
set typeMap to {%s}
repeat with pair in typeMap
	set utiType to item 1 of pair
	set fName to item 2 of pair
	if (pb's availableTypeFromArray:{utiType}) is not missing value then
		set d to (pb's dataForType:utiType)
		if d is not missing value and (d's |length|()) > 0 then
			d's writeToFile:(tmpDir & fName) atomically:true
		end if
	end if
end repeat`, dir, strings.Join(pairs, ", "))
})

func readClipboardDarwin(ctx context.Context) {
	_ = clipCmd(ctx, "osascript", "-e", darwinReadScript()).Run()
}

var darwinWriteScript = sync.OnceValue(func() string {
	dir := clipTmpDir()
	var pairs []string
	for _, e := range clipFormatTable {
		pairs = append(pairs, fmt.Sprintf(`{"%s", "/%s"}`, e.darwinUTI, e.fileName))
	}
	return fmt.Sprintf(`use framework "AppKit"
set pb to current application's NSPasteboard's generalPasteboard()
pb's clearContents()
set tmpDir to "%s"
set fm to current application's NSFileManager's defaultManager()
set typeMap to {%s}
repeat with pair in typeMap
	set utiType to item 1 of pair
	set fName to item 2 of pair
	set fp to tmpDir & fName
	if (fm's fileExistsAtPath:fp) as boolean then
		set d to current application's NSData's dataWithContentsOfFile:fp
		if d is not missing value then
			pb's setData:d forType:utiType
		end if
	end if
end repeat`, dir, strings.Join(pairs, ", "))
})

func writeClipboardDarwin(ctx context.Context) {
	_ = clipCmd(ctx, "osascript", "-e", darwinWriteScript()).Run()
}

// readClipboardPlatform reads clipboard formats on macOS into dir.
func readClipboardPlatform(ctx context.Context, dir string) {
	readClipboardDarwin(ctx)
}

// writeClipboardPlatform writes clipboard formats on macOS.
func writeClipboardPlatform(ctx context.Context, dir string, formats []*pb.ClipFormat, fmtMap map[string][]byte) {
	writeClipboardDarwin(ctx)
}

// loadPlatformFormats returns nil on macOS (no extra format extraction needed).
func loadPlatformFormats(dir string) []*pb.ClipFormat {
	return nil
}

// readFilesPlatform reads file paths from the macOS clipboard.
func readFilesPlatform(ctx context.Context) []string {
	script := `
	use framework "AppKit"
	set pb to current application's NSPasteboard's generalPasteboard()
	set fileType to current application's NSPasteboardTypeFileURL
	if (pb's availableTypeFromArray:{fileType}) is missing value then return ""
	set urls to pb's readObjectsForClasses:{current application's NSURL} options:(missing value)
	if urls is missing value then return ""
	set cnt to (urls's |count|()) as integer
	if cnt = 0 then return ""
	set pathList to ""
	repeat with i from 1 to cnt
		set u to (urls's objectAtIndex:(i - 1))
		if (u's isFileURL()) as boolean then
			set p to (u's |path|()) as text
			set pathList to pathList & p & linefeed
		end if
	end repeat
	return pathList`
	out, err := clipCmd(ctx, "osascript", "-e", script).Output()
	if err != nil {
		return nil
	}
	return parsePathList(string(out))
}

// writeFilesPlatform writes file paths to the macOS clipboard.
func writeFilesPlatform(ctx context.Context, paths []string) {
	var sb strings.Builder
	sb.WriteString("use framework \"AppKit\"\n")
	sb.WriteString("set pb to current application's NSPasteboard's generalPasteboard()\npb's clearContents()\n")
	sb.WriteString("set urls to current application's NSMutableArray's new()\n")
	for _, p := range paths {
		esc := strings.ReplaceAll(strings.ReplaceAll(p, "\\", "\\\\"), "\"", "\\\"")
		fmt.Fprintf(&sb, "urls's addObject:(current application's NSURL's fileURLWithPath:\"%s\")\n", esc)
	}
	sb.WriteString("pb's writeObjects:urls\n")
	_ = clipCmd(ctx, "osascript", "-e", sb.String()).Run()
}

// skipPerIfaceBroadcast returns false on macOS — per-interface broadcasts are safe.
func skipPerIfaceBroadcast() bool {
	return false
}
