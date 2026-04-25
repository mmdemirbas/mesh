package filesync

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// bakSuffix is the on-disk marker placed between the original path
// and the hash. The "*.mesh-bak-*" builtin ignore in ignore.go
// skips files containing this substring; cleanTempFiles ages out
// stragglers; the structured sweep at folder open (Phase I) handles
// fresh leftovers under explicit branches.
const bakSuffix = ".mesh-bak-"

// bakRelPath returns the F7 backup path for a given original
// (`<original>.mesh-bak-<hex(hash)>`). The hash names the OLD local
// content that the .bak preserves so the startup sweep (Phase I)
// can match the bak filename against the SQLite row's hash and
// decide whether to unlink (success path: SQLite carries the new
// content, the bak is stale) or restore (commit-failed path: SQLite
// still carries the original, the bak is the live local copy).
//
// Hex encoding (not base64 / base32) is intentional: it survives
// every filesystem in v1's target set (APFS, ext4, NTFS, FAT32),
// is case-insensitive-stable on the platforms that matter, and the
// 64-character width is well under any sane PATH_MAX after the
// original path is prepended.
func bakRelPath(relPath string, oldHash Hash256) string {
	return relPath + bakSuffix + hex.EncodeToString(oldHash[:])
}

// errBakRestoreFailed is returned by installDownloadedFile when the
// rollback path itself fails — both the new content and the .bak are
// in an inconsistent on-disk state and an operator must intervene.
// The wrapped error names the original commit failure that triggered
// the rollback so the operator can correlate the SQLite log line.
var errBakRestoreFailed = errors.New("filesync: F7 .bak restore failed; manual recovery required")

// installDownloadedFile applies a freshly-downloaded temp file as
// the new content for relPath, protecting against partial-failure
// with the F7 .mesh-bak-<hash> three-step pattern (audit §6 commit
// 6 phase E, closes Gap 4').
//
// Steps (all happen-before strictly ordered):
//  1. If a local file already exists at relPath, hash it and rename
//     to <relPath>.mesh-bak-<hash>. The rename is atomic on every
//     supported filesystem; an interrupt between hash and rename
//     leaves the original at its original name and the install
//     hasn't started yet.
//  2. Rename the freshly-downloaded temp file (tmpRelPath) to
//     relPath. After this rename, the new content is live on disk
//     but SQLite still carries the OLD content's hash.
//  3. Call commit. The callback owns the SQLite write that promotes
//     the new content into the peer-visible index — typically a
//     saveIndex with a single dirty row. commit MUST run
//     synchronously and return only when the SQLite COMMIT has
//     landed (or returned an error). The .bak window is open
//     between step 2 and step 4.
//  4. On commit success: unlink the .bak. On commit failure: rename
//     bak → relPath (clobbers the new content, restores the old
//     local copy), then unlink any straggler temp file. Bumps the
//     metric on success and on rollback so the dashboard surfaces
//     the lifecycle counters.
//
// Concurrency contract: the caller MUST hold claimPath(relPath) for
// the entire duration of this call (Phase F extends the claim to
// span steps 1-4). The walker check installed in scanWithStats then
// skips relPath while installation is in flight, closing the
// inverse race that scan would otherwise re-hash the new content
// during the .bak window.
//
// Error envelope: a returned error is always wrapped with
// fmt.Errorf("…: %w", inner) so callers can use errors.Is to
// distinguish errBakRestoreFailed (operator-action-required) from
// ordinary commit failures (retryable on next sync cycle). Step-1
// hash failures and step-1 rename failures both surface as ordinary
// errors with no on-disk side effects — the original is untouched.
func installDownloadedFile(
	root *os.Root,
	relPath, tmpRelPath string,
	commit func() error,
	metrics *FolderSyncMetrics,
) error {
	if root == nil {
		return fmt.Errorf("installDownloadedFile: nil root")
	}
	if commit == nil {
		return fmt.Errorf("installDownloadedFile: nil commit callback")
	}
	if relPath == "" || tmpRelPath == "" {
		return fmt.Errorf("installDownloadedFile: empty path: rel=%q tmp=%q", relPath, tmpRelPath)
	}
	if strings.Contains(relPath, bakSuffix) {
		return fmt.Errorf("installDownloadedFile: refusing to install over a .bak path: %s", relPath)
	}

	// Step 1: hash the existing local file (if any) and rename to .bak.
	var bakPath string
	hadOriginal := false
	if info, statErr := root.Stat(relPath); statErr == nil && !info.IsDir() {
		oldHash, hashErr := hashFileRoot(root, relPath)
		if hashErr != nil {
			return fmt.Errorf("hash original for backup: %w", hashErr)
		}
		bakPath = bakRelPath(relPath, oldHash)
		if err := root.Rename(relPath, bakPath); err != nil {
			return fmt.Errorf("rename original to backup: %w", err)
		}
		hadOriginal = true
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("stat original: %w", statErr)
	}

	// Step 2: rename temp → original. On failure, restore .bak (if
	// any) and surface the rename error. Do NOT unlink tmpRelPath
	// here — leave the temp on disk so cleanTempFiles or a manual
	// retry can recover it.
	if err := renameReplaceRoot(root, tmpRelPath, relPath); err != nil {
		if hadOriginal {
			if restoreErr := root.Rename(bakPath, relPath); restoreErr != nil {
				if metrics != nil {
					metrics.BakRestoreFailed.Add(1)
				}
				slog.Error("F7 install: rename temp→original failed AND .bak restore failed; manual recovery required",
					"path", relPath, "bak", bakPath, "tmp", tmpRelPath,
					"rename_error", err, "restore_error", restoreErr)
				return fmt.Errorf("rename temp→original (%v); %w (%v)",
					err, errBakRestoreFailed, restoreErr)
			}
		}
		return fmt.Errorf("rename temp to original: %w", err)
	}

	// Step 3: SQLite commit.
	if commitErr := commit(); commitErr != nil {
		// Step 5 (commit-failure rollback): rename bak → original
		// clobbers the new content with the old, restoring the
		// pre-download local state. The clobber is intentional —
		// the new content is rejected by SQLite and preserving it
		// would only widen the failure surface.
		if hadOriginal {
			if restoreErr := renameReplaceRoot(root, bakPath, relPath); restoreErr != nil {
				if metrics != nil {
					metrics.BakRestoreFailed.Add(1)
				}
				slog.Error("F7 install: SQLite commit failed AND .bak restore failed; manual recovery required",
					"path", relPath, "bak", bakPath,
					"commit_error", commitErr, "restore_error", restoreErr)
				return fmt.Errorf("commit (%v); %w (%v)",
					commitErr, errBakRestoreFailed, restoreErr)
			}
			if metrics != nil {
				metrics.BakRestoredOnCommitFail.Add(1)
			}
			slog.Warn("F7 install: SQLite commit failed; restored from .bak",
				"path", relPath, "error", commitErr)
		} else {
			// No prior original — the new file at relPath has no
			// pre-download counterpart. Remove it so the on-disk
			// state matches SQLite (which still has no row for the
			// path). Clean up any temp leftover for symmetry.
			_ = root.Remove(relPath)
			_ = root.Remove(tmpRelPath)
			if metrics != nil {
				metrics.BakRestoredOnCommitFail.Add(1)
			}
		}
		return fmt.Errorf("sqlite commit: %w", commitErr)
	}

	// Step 4: success. Unlink the .bak; failure to unlink is logged
	// but not surfaced — the structured sweep at folder open will
	// reconcile any straggler against the SQLite row (Phase I
	// branch a: SQLite carries the new hash → unlink the .bak).
	if hadOriginal {
		if err := root.Remove(bakPath); err != nil {
			slog.Warn("F7 install: succeeded but .bak unlink failed; sweep will reconcile",
				"path", relPath, "bak", bakPath, "error", err)
		}
	}
	return nil
}

// installDeletion removes a local file under the same F7 .bak
// protection that installDownloadedFile applies to downloads (audit
// §6 commit 6 phase G, INV-4 non-scan-path bullet for the delete
// half). The "new state" of a delete is "no file at relPath," so
// the .bak intermediate doubles as the staging spot for the removed
// content — a commit-failure rollback is just rename(bak, original).
//
// Steps:
//  1. If a local file exists at relPath, hash it and rename to
//     <relPath>.mesh-bak-<hash>. Disk now reflects the post-delete
//     state ("no file at relPath") with the old content preserved
//     under a sweep-recognized name.
//  2. Call commit. The callback owns the SQLite tombstone write —
//     typically a saveIndex with a single dirty row marking
//     Deleted=true.
//  3. On commit success: unlink the .bak. On failure: rename bak
//     back to relPath, restoring the on-disk state to pre-delete.
//
// If relPath does not exist locally, step 1 is a no-op and the
// callback runs immediately. A commit failure with no original
// returns the commit error verbatim — no on-disk state to roll
// back. Like installDownloadedFile this function does not perform
// its own claim management; the caller must hold claimPath(relPath)
// for the entire call so commit 5's scan walker skip covers the
// transient .bak window.
func installDeletion(
	root *os.Root,
	relPath string,
	commit func() error,
	metrics *FolderSyncMetrics,
) error {
	if root == nil {
		return fmt.Errorf("installDeletion: nil root")
	}
	if commit == nil {
		return fmt.Errorf("installDeletion: nil commit callback")
	}
	if relPath == "" {
		return fmt.Errorf("installDeletion: empty path")
	}
	if strings.Contains(relPath, bakSuffix) {
		return fmt.Errorf("installDeletion: refusing to delete a .bak path: %s", relPath)
	}

	var bakPath string
	hadOriginal := false
	if info, statErr := root.Stat(relPath); statErr == nil && !info.IsDir() {
		oldHash, hashErr := hashFileRoot(root, relPath)
		if hashErr != nil {
			return fmt.Errorf("hash original for delete-backup: %w", hashErr)
		}
		bakPath = bakRelPath(relPath, oldHash)
		if err := root.Rename(relPath, bakPath); err != nil {
			return fmt.Errorf("rename original to delete-backup: %w", err)
		}
		hadOriginal = true
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("stat original for delete: %w", statErr)
	}

	if commitErr := commit(); commitErr != nil {
		if hadOriginal {
			if restoreErr := renameReplaceRoot(root, bakPath, relPath); restoreErr != nil {
				if metrics != nil {
					metrics.BakRestoreFailed.Add(1)
				}
				slog.Error("F7 delete: SQLite commit failed AND .bak restore failed; manual recovery required",
					"path", relPath, "bak", bakPath,
					"commit_error", commitErr, "restore_error", restoreErr)
				return fmt.Errorf("delete commit (%v); %w (%v)",
					commitErr, errBakRestoreFailed, restoreErr)
			}
			if metrics != nil {
				metrics.BakRestoredOnCommitFail.Add(1)
			}
			slog.Warn("F7 delete: SQLite commit failed; restored from .bak",
				"path", relPath, "error", commitErr)
		}
		return fmt.Errorf("sqlite commit (delete): %w", commitErr)
	}

	if hadOriginal {
		if err := root.Remove(bakPath); err != nil {
			slog.Warn("F7 delete: succeeded but .bak unlink failed; sweep will reconcile",
				"path", relPath, "bak", bakPath, "error", err)
		}
	}
	return nil
}
