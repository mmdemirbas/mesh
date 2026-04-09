//go:build !darwin && !linux && !windows

package clipsync

import (
	"context"

	pb "github.com/mmdemirbas/mesh/internal/clipsync/proto"
)

func readClipboardPlatform(ctx context.Context, dir string)  {}
func writeClipboardPlatform(ctx context.Context, dir string, formats []*pb.ClipFormat, fmtMap map[string][]byte) {
}
func loadPlatformFormats(dir string) []*pb.ClipFormat { return nil }
func readFilesPlatform(ctx context.Context) []string   { return nil }
func writeFilesPlatform(ctx context.Context, paths []string) {}
func skipPerIfaceBroadcast() bool                            { return false }
