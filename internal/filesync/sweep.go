package filesync

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// bakSweepResult carries the structured outcome of one sweep pass
// over a folder root. Branches a and b (audit F7 sweep contract,
// §6 commit 6 phase I) increment Unlinked or Restored. Z13 (phase
// J) records the divergent paths in NeitherMatches so the caller
// can enter FolderDisabled(unknown) with a diagnostic message that
// names every offending path. Z1 (phase J) is signaled by the
// returned error — the caller leaves the .bak files intact and
// surfaces the SQLite-derived disabled reason.
type bakSweepResult struct {
	Scanned        int      // every *.mesh-bak-* file the walker visited
	Unlinked       int      // branch (a): SQLite carried the new hash → bak removed
	Restored       int      // branch (b): SQLite carried the original hash → bak rolled back into place
	Orphans        []string // .bak files whose original path has no SQLite row (no tombstone, no live row); treated as ageable strays
	NeitherMatches []string // Z13: original paths whose SQLite hash matches NEITHER the on-disk file NOR the bak filename hash
}

// errSweepDBUnreadable is returned by runStartupBakSweep when the
// folder's SQLite file is opened but a row query fails (Z1 — the DB
// is unreadable beyond the open path's PRAGMA quick_check). The
// caller MUST NOT touch any .bak files; the operator's restore-
// from-backup procedure runs the sweep again post-reopen. Lands in
// commit 6.2 phase J.
var errSweepDBUnreadable = errors.New("filesync: F7 sweep: SQLite unreadable; preserving .bak files for operator recovery")

// errSweepNeitherMatches is returned by runStartupBakSweep when at
// least one path's on-disk and bak-filename hashes both diverge
// from the SQLite row's hash (Z13). The caller transitions the
// folder to FolderDisabled(unknown) with a diagnostic payload that
// names the offending paths.
var errSweepNeitherMatches = errors.New("filesync: F7 sweep: neither disk file nor .bak matches SQLite hash")

// runStartupBakSweep walks the folder root for *.mesh-bak-* files
// (audit F7 sweep contract, §6 commit 6 phase I + phase J) and
// reconciles each against the SQLite row for the underlying path.
// Called from Node.Run after openFolderDB succeeds and PRAGMA
// quick_check passes; the structured branches are:
//
//	(a) SQLite row's hash == on-disk file's hash → unlink the .bak.
//	    The download lifecycle's commit-then-unlink succeeded but
//	    the unlink crashed; the .bak is stale.
//	(b) SQLite row's hash == .bak filename hash → rename bak →
//	    original (clobbering whatever is currently at the original
//	    path). The download lifecycle's SQLite commit failed (or
//	    the process crashed before the commit landed); SQLite still
//	    carries the pre-download content, the .bak holds the live
//	    local copy, restoring it converges on-disk and SQLite.
//	(Z1) Any per-row SQLite query fails → return errSweepDBUnreadable
//	     immediately, leaving every visited .bak intact. The folder
//	     transitions to FolderDisabled with the SQLite-derived
//	     reason; restore-from-backup re-runs the sweep against the
//	     restored DB after reopen.
//	(Z13) On-disk file's hash AND .bak filename hash both diverge
//	      from SQLite → record the original path in NeitherMatches.
//	      The caller transitions the folder to FolderDisabled
//	      reason `unknown` with a diagnostic payload that names
//	      every NeitherMatches path (commit 3's diagnostic-load
//	      machinery handles the inline payload for `unknown`).
//
// Tombstone rows (deleted=1): treated as branch (a) when SQLite's
// hash matches the bak filename — Phase G's installDeletion left
// the bak in place when the tombstone commit succeeded but the
// unlink crashed. We don't try to "restore" a tombstoned file;
// once SQLite says deleted, the .bak is unconditionally stale.
//
// Orphans (no row at all): the .bak refers to a path SQLite has
// never heard of. Could be a leftover from a removed-and-re-added
// folder, an externally-introduced .bak-shaped file, or an aged
// stray from before the F7 lifecycle existed. Recorded but not
// touched — the existing age-gated cleanTempFiles handles them on
// the slow path.
func runStartupBakSweep(ctx context.Context, db *sql.DB, root *os.Root, folderID string) (bakSweepResult, error) {
	var result bakSweepResult
	if db == nil || root == nil {
		return result, fmt.Errorf("runStartupBakSweep: nil db or root")
	}
	rootPath := root.Name()

	walkErr := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		base := d.Name()
		idx := strings.Index(base, bakSuffix)
		if idx < 0 {
			return nil
		}
		// Filename layout: <orig-base> + ".mesh-bak-" + <hex hash>.
		// 32-byte SHA-256 hex-encoded is exactly 64 chars. Defensive
		// against malformed names — anything else is an external
		// file that happens to contain ".mesh-bak-" and is left
		// untouched.
		hashHex := base[idx+len(bakSuffix):]
		if len(hashHex) != 64 {
			return nil
		}
		var bakHash Hash256
		if _, hexErr := hex.Decode(bakHash[:], []byte(hashHex)); hexErr != nil {
			return nil
		}
		origBase := base[:idx]
		if origBase == "" {
			return nil
		}

		relBakRaw, relErr := filepath.Rel(rootPath, path)
		if relErr != nil {
			return nil
		}
		relBak := filepath.ToSlash(relBakRaw)
		relOrig := filepath.ToSlash(filepath.Join(filepath.Dir(relBakRaw), origBase))
		result.Scanned++

		// Look up the SQLite row for the original path. tombstones
		// (deleted=1) are treated like live rows whose hash is the
		// pre-tombstone content — installDeletion's bak filename
		// names exactly that hash, so branch (b) (restore) correctly
		// fires when the delete-commit crashed and we should bring
		// the old content back. Phase G's flow names that recovery
		// path explicitly.
		var sqlHashBytes []byte
		var deletedInt int64
		queryErr := db.QueryRowContext(ctx,
			`SELECT hash, deleted FROM files WHERE folder_id=? AND path=?`,
			folderID, relOrig,
		).Scan(&sqlHashBytes, &deletedInt)
		switch {
		case errors.Is(queryErr, sql.ErrNoRows):
			result.Orphans = append(result.Orphans, relBak)
			return nil
		case queryErr != nil:
			// Z1: any per-row failure is fatal for the sweep.
			// Wrap rather than stop the walker — the caller checks
			// for errSweepDBUnreadable via errors.Is.
			return fmt.Errorf("%w: row query for %s: %v",
				errSweepDBUnreadable, relOrig, queryErr)
		}
		if len(sqlHashBytes) != len(bakHash) {
			return fmt.Errorf("%w: row %s has hash of %d bytes, want %d",
				errSweepDBUnreadable, relOrig, len(sqlHashBytes), len(bakHash))
		}
		var sqlHash Hash256
		copy(sqlHash[:], sqlHashBytes)

		// A tombstone row means the path SHOULD have no live file —
		// the .bak is the artifact of installDeletion's pre-commit
		// rename. Whether the bak's hash matches the tombstone's
		// hash or not, the live state is "no file at relOrig":
		//   - bak matches tombstone hash: the deletion was
		//     committed cleanly, the bak unlink crashed → unlink it.
		//   - bak does not match: stale .bak from a previous
		//     install attempt → unlink it as well; the current
		//     SQLite state is authoritative.
		// Either way, branch (a) is the right answer. We do NOT
		// restore a tombstoned file even if the bak claims to hold
		// the pre-tombstone content; that would resurrect a path
		// peers have already converged on.
		if deletedInt != 0 {
			if err := root.Remove(relBak); err != nil {
				slog.Warn("F7 sweep: tombstoned-row .bak unlink failed",
					"folder", folderID, "path", relOrig, "bak", relBak, "error", err)
			} else {
				result.Unlinked++
			}
			return nil
		}

		// Branch (b): SQLite still has the original (pre-download)
		// content. Restore the .bak — clobbers whatever is at
		// relOrig today, which is either the post-rename new
		// content (commit failed) or nothing (rare; the rename
		// completed but neither the commit nor the new file
		// survived). Restoring overwrites either way.
		if sqlHash == bakHash {
			if err := renameReplaceRoot(root, relBak, relOrig); err != nil {
				return fmt.Errorf("F7 sweep: restore %s from .bak: %w", relOrig, err)
			}
			result.Restored++
			slog.Info("F7 sweep: restored .bak after crash",
				"folder", folderID, "path", relOrig)
			return nil
		}

		// Branch (a) or Z13. Hash the on-disk file at relOrig.
		// If SQLite's hash matches the on-disk file, the install
		// succeeded and only the bak unlink missed — branch (a).
		origExists := false
		var origHash Hash256
		if h, hErr := hashFileRoot(root, relOrig); hErr == nil {
			origHash = h
			origExists = true
		} else if !os.IsNotExist(hErr) {
			// Read failure on a path SQLite knows about. Treat as
			// Z1 — we can't safely classify without knowing the
			// content.
			return fmt.Errorf("%w: hash %s: %v", errSweepDBUnreadable, relOrig, hErr)
		}

		if origExists && origHash == sqlHash {
			if err := root.Remove(relBak); err != nil {
				slog.Warn("F7 sweep: branch-a .bak unlink failed",
					"folder", folderID, "path", relOrig, "bak", relBak, "error", err)
			} else {
				result.Unlinked++
			}
			return nil
		}

		// Z13: neither the on-disk file (if any) nor the bak's
		// filename hash matches SQLite. Record the path; the
		// caller surfaces it via FolderDisabled(unknown).
		result.NeitherMatches = append(result.NeitherMatches, relOrig)
		return nil
	})

	if walkErr != nil {
		return result, walkErr
	}
	if len(result.NeitherMatches) > 0 {
		return result, fmt.Errorf("%w: %d path(s)", errSweepNeitherMatches, len(result.NeitherMatches))
	}
	return result, nil
}
