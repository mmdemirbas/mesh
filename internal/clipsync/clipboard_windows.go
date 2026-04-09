//go:build windows

package clipsync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	pb "github.com/mmdemirbas/mesh/internal/clipsync/proto"
)

var windowsReadScript = sync.OnceValue(func() string {
	dir := clipTmpDir()
	return fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$d = '%s'
$text = [System.Windows.Forms.Clipboard]::GetText([System.Windows.Forms.TextDataFormat]::UnicodeText)
if ($text) { [System.IO.File]::WriteAllBytes("$d\text_plain", [System.Text.Encoding]::UTF8.GetBytes($text)) }
if ([System.Windows.Forms.Clipboard]::ContainsData('HTML Format')) {
  $obj = [System.Windows.Forms.Clipboard]::GetData('HTML Format')
  if ($obj -is [System.IO.MemoryStream]) {
    $r = New-Object System.IO.StreamReader($obj, [System.Text.Encoding]::UTF8); $cf = $r.ReadToEnd()
    [System.IO.File]::WriteAllText("$d\text_html_cf", $cf, [System.Text.Encoding]::UTF8)
  } elseif ($obj -is [string]) {
    [System.IO.File]::WriteAllText("$d\text_html_cf", $obj, [System.Text.Encoding]::UTF8)
  }
}
if ([System.Windows.Forms.Clipboard]::ContainsData([System.Windows.Forms.DataFormats]::Rtf)) {
  $rtf = [System.Windows.Forms.Clipboard]::GetData([System.Windows.Forms.DataFormats]::Rtf)
  if ($rtf -is [string]) { [System.IO.File]::WriteAllBytes("$d\text_rtf", [System.Text.Encoding]::UTF8.GetBytes($rtf)) }
}
$img = [System.Windows.Forms.Clipboard]::GetImage()
if ($img) {
  $ms = New-Object System.IO.MemoryStream; $img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
  [System.IO.File]::WriteAllBytes("$d\image_png", $ms.ToArray()); $ms.Dispose(); $img.Dispose()
}`, dir)
})

func readClipboardWindows(ctx context.Context) {
	// Run powershell in a fresh goroutine to prevent the long-lived pollClipboard goroutine
	// from accumulating a corrupted syscall.StartProcess stack frame (Go runtime bug on
	// Windows where stack reallocation for CGo-path frames can corrupt return addresses,
	// causing GC to crash with "unexpected return pc for syscall.StartProcess").
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = clipCmd(ctx, "powershell", "-NoProfile", "-STA", "-Command", windowsReadScript()).Run()
	}()
	<-done
}

var windowsWriteScript = sync.OnceValue(func() string {
	dir := clipTmpDir()
	return fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$d = '%s'
$dataObj = New-Object System.Windows.Forms.DataObject
$fp = "$d\text_plain"
if (Test-Path $fp) { $dataObj.SetText([System.IO.File]::ReadAllText($fp, [System.Text.Encoding]::UTF8)) }
$fp = "$d\text_html_cf"
if (Test-Path $fp) {
  $bytes = [System.IO.File]::ReadAllBytes($fp)
  $ms = New-Object System.IO.MemoryStream(,$bytes)
  $dataObj.SetData('HTML Format', $ms)
}
$fp = "$d\text_rtf"
if (Test-Path $fp) { $dataObj.SetData([System.Windows.Forms.DataFormats]::Rtf, [System.IO.File]::ReadAllText($fp, [System.Text.Encoding]::UTF8)) }
$fp = "$d\image_png"
if (Test-Path $fp) {
  $bytes = [System.IO.File]::ReadAllBytes($fp)
  $ms = New-Object System.IO.MemoryStream(,$bytes)
  $img = [System.Drawing.Image]::FromStream($ms)
  $dataObj.SetImage($img)
}
[System.Windows.Forms.Clipboard]::SetDataObject($dataObj, $true)`, dir)
})

func writeClipboardWindows(ctx context.Context, fmtMap map[string][]byte) {
	// HTML needs CF_HTML wrapping.
	if html, ok := fmtMap["text/html"]; ok {
		cfhtml := buildCFHTML(string(html))
		_ = os.WriteFile(filepath.Join(clipTmpDir(), "text_html_cf"), []byte(cfhtml), 0600)
	}
	// Same goroutine isolation as readClipboardWindows — see comment there.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = clipCmd(ctx, "powershell", "-NoProfile", "-STA", "-Command", windowsWriteScript()).Run()
	}()
	<-done
}

// readClipboardPlatform reads clipboard formats on Windows into dir.
func readClipboardPlatform(ctx context.Context, dir string) {
	readClipboardWindows(ctx)
}

// writeClipboardPlatform writes clipboard formats on Windows.
func writeClipboardPlatform(ctx context.Context, dir string, formats []*pb.ClipFormat, fmtMap map[string][]byte) {
	writeClipboardWindows(ctx, fmtMap)
}

// loadPlatformFormats extracts the CF_HTML fragment from the Windows clipboard temp dir.
func loadPlatformFormats(dir string) []*pb.ClipFormat {
	cfdata, err := os.ReadFile(filepath.Join(dir, "text_html_cf")) //nolint:gosec // G304: dir is the node's private filesDir; filename is a fixed constant
	if err != nil || len(cfdata) == 0 {
		return nil
	}
	if frag := extractCFHTMLFragment(string(cfdata)); frag != "" {
		return []*pb.ClipFormat{{MimeType: "text/html", Data: []byte(frag)}}
	}
	return nil
}

// readFilesPlatform reads file paths from the Windows clipboard.
func readFilesPlatform(ctx context.Context) []string {
	script := `(Get-Clipboard -Format FileDropList).FullName`
	out, err := clipCmd(ctx, "powershell", "-NoProfile", "-Command", script).Output()
	if err != nil {
		return nil
	}
	return parsePathList(string(out))
}

// writeFilesPlatform writes file paths to the Windows clipboard.
func writeFilesPlatform(ctx context.Context, paths []string) {
	var sb strings.Builder
	sb.WriteString("Set-Clipboard -Path ")
	for i, p := range paths {
		// PowerShell string escape to prevent command injection
		safePath := strings.ReplaceAll(p, "'", "''")
		fmt.Fprintf(&sb, "'%s'", safePath)
		if i < len(paths)-1 {
			sb.WriteString(",")
		}
	}
	_ = clipCmd(ctx, "powershell", "-NoProfile", "-Command", sb.String()).Run()
}

// skipPerIfaceBroadcast returns true on Windows — per-interface UDP broadcasts
// can crash Windows network drivers; the global 255.255.255.255 is stable.
func skipPerIfaceBroadcast() bool {
	return true
}
