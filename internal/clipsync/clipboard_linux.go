//go:build linux

package clipsync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"os/exec"

	pb "github.com/mmdemirbas/mesh/internal/clipsync/proto"
)

// linuxClipTool caches the detected Linux clipboard tool at first use.
// The available tool won't change during process lifetime, so calling
// exec.LookPath on every 500ms poll tick is wasteful.
var linuxClipTool struct {
	once sync.Once
	name string // "xclip", "wl" (for wl-clipboard), or "" (none)
}

func detectLinuxClipTool() string {
	linuxClipTool.once.Do(func() {
		if _, err := exec.LookPath("xclip"); err == nil {
			linuxClipTool.name = "xclip"
		} else if _, err := exec.LookPath("wl-paste"); err == nil {
			linuxClipTool.name = "wl"
		}
	})
	return linuxClipTool.name
}

func readClipboardLinux(ctx context.Context, dir string) {
	tool := detectLinuxClipTool()
	if tool == "" {
		return
	}

	// Discover which MIME types are available on the clipboard.
	var targetsCmd *exec.Cmd
	switch tool {
	case "xclip":
		targetsCmd = clipCmd(ctx, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
	case "wl":
		targetsCmd = clipCmd(ctx, "wl-paste", "--list-types")
	}
	targetsOut, err := targetsCmd.Output()
	if err != nil {
		return
	}
	available := string(targetsOut)

	type linuxTarget struct {
		target   string // X11/Wayland MIME target
		fileName string
	}
	known := []linuxTarget{
		{"UTF8_STRING", "text_plain"},
		{"text/html", "text_html"},
		{"text/rtf", "text_rtf"},
		{"image/png", "image_png"},
	}
	// wl-paste uses standard MIME types instead of X11 atoms.
	if tool == "wl" {
		known[0] = linuxTarget{"text/plain", "text_plain"}
	}

	for _, kt := range known {
		if !strings.Contains(available, kt.target) {
			continue
		}
		var cmd *exec.Cmd
		switch tool {
		case "xclip":
			cmd = clipCmd(ctx, "xclip", "-selection", "clipboard", "-t", kt.target, "-o")
		case "wl":
			cmd = clipCmd(ctx, "wl-paste", "-t", kt.target)
		}
		data, err := cmd.Output()
		if err == nil && len(data) > 0 {
			_ = os.WriteFile(filepath.Join(dir, kt.fileName), data, 0600)
		}
	}
}

func writeClipboardLinux(ctx context.Context, formats []*pb.ClipFormat) {
	// Linux clipboard tools can only set one MIME type per invocation.
	// Write the most universally useful format.
	priority := []string{"text/plain", "text/html", "text/rtf", "image/png", "image/tiff"}
	for _, mime := range priority {
		for _, f := range formats {
			if f.GetMimeType() != mime {
				continue
			}
			tool := detectLinuxClipTool()
			var cmd *exec.Cmd
			switch tool {
			case "xclip":
				target := mime
				if mime == "text/plain" {
					target = "UTF8_STRING"
				}
				cmd = clipCmd(ctx, "xclip", "-selection", "clipboard", "-t", target, "-i")
			case "wl":
				cmd = clipCmd(ctx, "wl-copy", "--type", mime)
			default:
				return
			}
			cmd.Stdin = strings.NewReader(string(f.GetData()))
			_ = cmd.Run()
			return
		}
	}
}

// readClipboardPlatform reads clipboard formats on Linux into dir.
func readClipboardPlatform(ctx context.Context, dir string) {
	readClipboardLinux(ctx, dir)
}

// writeClipboardPlatform writes clipboard formats on Linux.
func writeClipboardPlatform(ctx context.Context, dir string, formats []*pb.ClipFormat, fmtMap map[string][]byte) {
	writeClipboardLinux(ctx, formats)
}

// loadPlatformFormats returns nil on Linux (no extra format extraction needed).
func loadPlatformFormats(dir string) []*pb.ClipFormat {
	return nil
}

// readFilesPlatform reads file paths from the Linux clipboard.
func readFilesPlatform(ctx context.Context) []string {
	var out []byte
	switch detectLinuxClipTool() {
	case "xclip":
		out, _ = clipCmd(ctx, "xclip", "-selection", "clipboard", "-t", "text/uri-list", "-o").Output()
	case "wl":
		out, _ = clipCmd(ctx, "wl-paste", "--type", "text/uri-list").Output()
	}
	if len(out) == 0 {
		return nil
	}
	return parseURIList(string(out))
}

// writeFilesPlatform writes file paths to the Linux clipboard.
func writeFilesPlatform(ctx context.Context, paths []string) {
	var sb strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&sb, "file://%s\r\n", p)
	}
	uriList := sb.String()

	var cmd *exec.Cmd
	switch detectLinuxClipTool() {
	case "xclip":
		cmd = clipCmd(ctx, "xclip", "-selection", "clipboard", "-t", "text/uri-list", "-i")
	case "wl":
		cmd = clipCmd(ctx, "wl-copy", "--type", "text/uri-list")
	default:
		return
	}
	cmd.Stdin = strings.NewReader(uriList)
	_ = cmd.Run()
}

// skipPerIfaceBroadcast returns false on Linux — per-interface broadcasts are safe.
func skipPerIfaceBroadcast() bool {
	return false
}
