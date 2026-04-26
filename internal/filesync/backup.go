package filesync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// backupFilePerm is the on-disk mode for VACUUM INTO output. 0600
// keeps the backup readable only by the user account that runs
// mesh — backups carry the same row data as the live index, so
// the same trust boundary applies. Audit §6 commit 9a / iter-4 O18.
const backupFilePerm os.FileMode = 0o600

// backupTmpSuffix marks a backup file as in-flight. Audit §6
// commit 9a / iter-4 Z5: VACUUM INTO writes to <name>.tmp first,
// then quick_check on the .tmp, then atomic rename to <name>;
// crash mid-write leaves a .tmp that the startup sweep cleans.
const backupTmpSuffix = ".tmp"

// backupFileRegex matches the production filename layout
// `index-<seq>-<unixns>.sqlite`. <seq> is the highest committed
// folder_meta.sequence at backup time; <unixns> is the wall clock
// at backup time, encoded as Unix nanoseconds (iter-4 O18). The
// regex is anchored to the entire basename so neither prefix nor
// suffix tolerates drift.
var backupFileRegex = regexp.MustCompile(`^index-(\d+)-(\d+)\.sqlite$`)

// BackupInfo describes one persisted backup file. Returned by the
// listing endpoint and used by the retention pruner.
type BackupInfo struct {
	Path         string    `json:"path"`
	Sequence     int64     `json:"sequence"`
	CreatedAt    time.Time `json:"created_at"`
	QuickCheckOK bool      `json:"quick_check_ok"`
}

// backupDirFor returns the per-folder backup directory path. Lives
// next to the folder's index.sqlite so backups never need to cross
// filesystems for the atomic rename in writeBackup.
func backupDirFor(folderCacheDir string) string {
	return filepath.Join(folderCacheDir, "backups")
}

// backupFinalName builds the final filename for a backup at the
// given (sequence, time). The corresponding .tmp lives at
// backupFinalName(...) + backupTmpSuffix.
func backupFinalName(seq int64, when time.Time) string {
	return fmt.Sprintf("index-%d-%d.sqlite", seq, when.UnixNano())
}

// writeBackup runs `VACUUM INTO` against db with the backup target
// computed from the highest committed folder_meta.sequence and the
// current wall clock. Audit §6 commit 9a / iter-4 Z5: writes to
// `.tmp` first, runs `quick_check` on the result, atomic-renames to
// the final filename only on pass. On failure or crash, the
// `.tmp` is left for the startup sweep (cleanBackupTmp) to remove.
//
// Returns the final BackupInfo on success. The wall-clock time is
// captured BEFORE VACUUM INTO so the filename's <unixns> reflects
// the start time, not the (variable) post-VACUUM time — the
// caller can then sort by filename and trust the ordering matches
// commit history.
//
// Concurrency: VACUUM INTO holds a read lock on the source DB, so
// the writer can keep committing during a backup. Caller is
// responsible for serializing concurrent writeBackup invocations
// against the same folder (the goroutine in scheduleBackups owns
// this contract).
func writeBackup(ctx context.Context, db *sql.DB, folderCacheDir string) (BackupInfo, error) {
	var info BackupInfo
	if db == nil {
		return info, fmt.Errorf("writeBackup: nil db")
	}
	dir := backupDirFor(folderCacheDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return info, fmt.Errorf("create backup dir: %w", err)
	}

	// Capture sequence + clock BEFORE the VACUUM so the filename
	// reflects when the backup STARTED. Reading folder_meta from
	// outside a tx is fine — folder_meta.sequence is monotonic and
	// any concurrent commit only bumps it; the backup will reflect
	// the post-VACUUM state with a slightly older filename, which
	// is safe (the sequence is a lower bound on what's inside).
	//
	// Missing-sequence-row case: seedFolderMeta does NOT insert a
	// sequence row at folder open; the first saveIndex call does.
	// A backup taken on a fresh folder before any scan / sync has
	// committed sees sql.ErrNoRows. Treat as sequence=0 — the
	// backup is still valid (an empty index is a real state) and
	// the filename will sort-first under listBackups' sequence-
	// descending order, which is correct.
	var seqStr string
	queryErr := db.QueryRowContext(ctx, `SELECT value FROM folder_meta WHERE key='sequence'`).Scan(&seqStr)
	var seq int64
	switch {
	case errors.Is(queryErr, sql.ErrNoRows):
		seq = 0
	case queryErr != nil:
		return info, fmt.Errorf("read folder_meta.sequence: %w", queryErr)
	default:
		s, err := parseInt64(seqStr)
		if err != nil {
			return info, fmt.Errorf("parse folder_meta.sequence: %w", err)
		}
		seq = s
	}
	startedAt := time.Now().UTC()

	finalName := backupFinalName(seq, startedAt)
	tmpPath := filepath.Join(dir, finalName+backupTmpSuffix)
	finalPath := filepath.Join(dir, finalName)

	// Pre-emptively unlink any leftover .tmp at the same name. In
	// the unlikely event of a same-nanosecond collision (e.g. two
	// backup goroutines firing in lock-step on a clock-skipping
	// host), this lets the second writer take the slot rather
	// than fail on VACUUM INTO's "file exists" branch.
	_ = os.Remove(tmpPath)

	// VACUUM INTO target. SQLite escapes the path internally; we
	// pass it as a parameter to keep the SQL string constant.
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", tmpPath); err != nil {
		_ = os.Remove(tmpPath) // best-effort: VACUUM INTO leaves a partial file on error
		return info, fmt.Errorf("VACUUM INTO %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, backupFilePerm); err != nil {
		_ = os.Remove(tmpPath)
		return info, fmt.Errorf("chmod backup .tmp: %w", err)
	}

	// quick_check the .tmp before promoting. Open a fresh sql.DB
	// for the check — using the production driver path keeps the
	// pragma behavior aligned with what runtime open does.
	checkDB, err := sql.Open("sqlite", tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return info, fmt.Errorf("open backup .tmp for quick_check: %w", err)
	}
	checkErr := runQuickCheck(checkDB)
	_ = checkDB.Close()
	if checkErr != nil {
		_ = os.Remove(tmpPath)
		return info, fmt.Errorf("backup quick_check: %w", checkErr)
	}

	// Atomic rename to final filename. After this, the backup is
	// visible to the listing endpoint and the retention pruner.
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return info, fmt.Errorf("rename backup to final: %w", err)
	}
	return BackupInfo{
		Path:         finalPath,
		Sequence:     seq,
		CreatedAt:    startedAt,
		QuickCheckOK: true,
	}, nil
}

// listBackups reads backupDirFor and returns one BackupInfo per
// final (non-.tmp) file whose name matches backupFileRegex. Sorted
// by Sequence descending so the most recent is first; ties are
// broken by CreatedAt descending so a same-sequence retry-after-
// crash still surfaces the newer one first. Audit §6 commit 9a /
// iter-4 O9.
//
// QuickCheckOK is set to true for files that exist (the write
// path's quick_check gates the rename to final, so any file in
// the listing has passed at backup time). The dashboard should
// surface a re-check on demand if the operator suspects the
// backup has rotted in storage.
func listBackups(folderCacheDir string) ([]BackupInfo, error) {
	dir := backupDirFor(folderCacheDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read backup dir: %w", err)
	}
	out := make([]BackupInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, backupTmpSuffix) {
			// Retention prune treats .tmp as invisible (audit
			// §6 commit 9a). Cleanup happens via cleanBackupTmp
			// at folder open / restart.
			continue
		}
		match := backupFileRegex.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		seq, _ := strconv.ParseInt(match[1], 10, 64)
		ns, _ := strconv.ParseInt(match[2], 10, 64)
		out = append(out, BackupInfo{
			Path:         filepath.Join(dir, name),
			Sequence:     seq,
			CreatedAt:    time.Unix(0, ns).UTC(),
			QuickCheckOK: true,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sequence != out[j].Sequence {
			return out[i].Sequence > out[j].Sequence
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// gfsRetention is the GFS (grandfather-father-son) policy for
// backup pruning. Audit §6 commit 9a / decision §5 #11. Five
// daily slots (most-recent of each of the last 5 calendar days),
// four weekly slots (most-recent of each of the last 4 ISO weeks
// not already represented in daily), one monthly slot (most-
// recent of any calendar month not already represented).
//
// Retention is computed against the full file list each prune;
// adding a backup that's already represented by a higher tier
// (e.g. today's daily slot also satisfies this week's weekly)
// does not consume an extra slot. Iter-4 Z14: pruning is
// idempotent — running the prune twice on the same input set
// produces the same kept set.
type gfsRetention struct {
	dailySlots   int
	weeklySlots  int
	monthlySlots int
}

// defaultGFS is the v1 retention policy. 5 daily + 4 weekly + 1
// monthly = 10 backup slots maximum (fewer if the folder has been
// running for less than a month).
var defaultGFS = gfsRetention{dailySlots: 5, weeklySlots: 4, monthlySlots: 1}

// gfsKeep walks the sorted-newest-first backup list and returns
// the set of paths to keep under the GFS policy. The remaining
// paths (returned by gfsPrune) are safe to unlink.
//
// Selection algorithm (audit §6 commit 9a / decision §5 #11):
//  1. Walk newest → oldest.
//  2. For each backup, compute day-key (calendar date), week-key
//     (ISO week), and month-key (calendar month).
//  3. The DAILY tier keeps the most-recent backup of each new
//     day, up to dailySlots distinct days. The newest backup
//     for a day is always preferred — already-seen days are
//     skipped at this tier.
//  4. The WEEKLY tier considers only weeks NOT already
//     represented by a daily-kept backup, up to weeklySlots
//     distinct new weeks. This prevents the daily and weekly
//     tiers from double-counting the same backup.
//  5. The MONTHLY tier considers only months NOT already
//     represented by daily or weekly, up to monthlySlots.
//
// The result: with 5 daily + 4 weekly + 1 monthly, exactly 10
// distinct backups can be kept (covering 10 distinct
// week-month combinations).
//
// nowFn is reserved for future "anchor on real-now" extensions.
// Today's algorithm uses only the BackupInfo.CreatedAt field;
// the clock is not consulted for slot-counting, but tests pass
// it for parity with future evolution.
func gfsKeep(backups []BackupInfo, policy gfsRetention, nowFn func() time.Time) map[string]bool {
	keep := make(map[string]bool, len(backups))
	dailySeen := make(map[string]bool)
	weeklySeen := make(map[string]bool)
	monthlySeen := make(map[string]bool)
	dailyKept, weeklyKept, monthlyKept := 0, 0, 0

	dayKey := func(t time.Time) string {
		y, m, d := t.Date()
		return fmt.Sprintf("%04d-%02d-%02d", y, m, d)
	}
	weekKey := func(t time.Time) string {
		y, w := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	}
	monthKey := func(t time.Time) string {
		y, m, _ := t.Date()
		return fmt.Sprintf("%04d-%02d", y, m)
	}

	for _, b := range backups {
		dk := dayKey(b.CreatedAt)
		wk := weekKey(b.CreatedAt)
		mk := monthKey(b.CreatedAt)

		// Daily tier: keep the most-recent backup per new day.
		if dailyKept < policy.dailySlots && !dailySeen[dk] {
			dailySeen[dk] = true
			weeklySeen[wk] = true  // daily-covered week — weekly tier skips it
			monthlySeen[mk] = true // daily-covered month — monthly tier skips it
			dailyKept++
			keep[b.Path] = true
			continue
		}
		// Weekly tier: only weeks NOT already covered by daily.
		if weeklyKept < policy.weeklySlots && !weeklySeen[wk] {
			weeklySeen[wk] = true
			monthlySeen[mk] = true // weekly-covered month — monthly tier skips it
			weeklyKept++
			keep[b.Path] = true
			continue
		}
		// Monthly tier: only months NOT already covered.
		if monthlyKept < policy.monthlySlots && !monthlySeen[mk] {
			monthlySeen[mk] = true
			monthlyKept++
			keep[b.Path] = true
			continue
		}
	}
	_ = nowFn // reserved for future "anchor on real-now" extensions
	return keep
}

// gfsPrune removes backup files NOT selected by gfsKeep. Returns
// the number of files unlinked. Iter-4 Z14: pruning is
// idempotent — running this twice on the same directory removes 0
// files on the second run because all .tmp files are invisible to
// the prune (treated as already-removed by listBackups) and the
// keep set is computed from the same input.
//
// Errors on individual unlinks are logged but never abort the
// loop — the goal is to free disk; one stuck file should not
// block the rest.
func gfsPrune(folderCacheDir, folderID string, policy gfsRetention, nowFn func() time.Time) (int, error) {
	backups, err := listBackups(folderCacheDir)
	if err != nil {
		return 0, fmt.Errorf("list backups: %w", err)
	}
	keep := gfsKeep(backups, policy, nowFn)
	pruned := 0
	for _, b := range backups {
		if keep[b.Path] {
			continue
		}
		if err := os.Remove(b.Path); err != nil {
			slog.Warn("gfsPrune: remove failed",
				"folder", folderID, "path", b.Path, "error", err)
			continue
		}
		pruned++
	}
	return pruned, nil
}

// cleanBackupTmp removes any `*.sqlite.tmp` strays from the backup
// directory. Called from Node.Run after openFolderDB succeeds and
// the F7 sweep completes. Audit §6 commit 9a — the startup sweep
// extension that closes the iter-4 Z5 atomic-write contract.
//
// Errors during readdir or per-file Remove are logged at debug —
// a stale .tmp is a perf concern (wastes disk), not a correctness
// one, and the next backup run will overwrite it anyway.
func cleanBackupTmp(folderCacheDir, folderID string) {
	dir := backupDirFor(folderCacheDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Debug("cleanBackupTmp: readdir failed", "folder", folderID, "dir", dir, "error", err)
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), backupTmpSuffix) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if err := os.Remove(full); err != nil {
			slog.Debug("cleanBackupTmp: remove failed",
				"folder", folderID, "path", full, "error", err)
		}
	}
}
