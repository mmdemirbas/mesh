package filesync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDisabledReasonActions_AllEnumsHaveAction pins iter-4 Z12 (decision
// §5 #28): every closed-enum DisabledReason value must have an action
// string in disabledReasonActions. The dashboard renderer and the API
// response both consume the map; a missing entry would render an
// empty action cell and confuse the operator.
//
// AllDisabledReasons is the iteration order; adding a new enum value
// requires updating the slice and the map together.
func TestDisabledReasonActions_AllEnumsHaveAction(t *testing.T) {
	t.Parallel()
	for _, r := range AllDisabledReasons() {
		action := disabledReasonActions[r]
		if action == "" {
			t.Errorf("missing action string for DisabledReason %q", string(r))
		}
	}
}

// TestRunbookActionStringsMatchMap pins iter-4 Z15 (decision §5 #28):
// the runbook §4 prose cites the action strings verbatim. A drift
// where the map updates but the runbook does not (or vice versa)
// shows the operator one thing in the dashboard and a different
// thing in the doc — exactly the operator-confusion failure §4.6 D5
// guards against.
//
// The test reads docs/filesync/OPERATOR-RUNBOOK.md and asserts every
// action string appears verbatim somewhere in the file.
func TestRunbookActionStringsMatchMap(t *testing.T) {
	t.Parallel()
	// Locate the runbook: the test runs from internal/filesync/, so
	// the runbook lives ../../docs/filesync/OPERATOR-RUNBOOK.md.
	candidates := []string{
		"../../docs/filesync/OPERATOR-RUNBOOK.md",
		"docs/filesync/OPERATOR-RUNBOOK.md",
	}
	var data []byte
	var err error
	for _, p := range candidates {
		data, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Skipf("OPERATOR-RUNBOOK.md not found relative to test cwd: %v", err)
	}
	doc := string(data)
	for r, action := range disabledReasonActions {
		// Skip the unknown reason — its action ("escalate per
		// runbook §4.8") is more directional than verbatim, and the
		// runbook §4.8 text describes the escalation procedure
		// without quoting the action string.
		if r == DisabledUnknown {
			continue
		}
		if !strings.Contains(doc, action) {
			t.Errorf("runbook does not contain verbatim action for %q\n  action: %q\n  fix: copy the action string into OPERATOR-RUNBOOK.md §4 (or update the map)",
				string(r), action)
		}
	}
}

// TestDeviceIDMismatch_DisablesFolder pins iter-3 I7 / audit decision
// §5 #20: opening a folder whose stored folder_meta.device_id differs
// from the node-level identity must transition the folder to
// FolderDisabled with reason `device_id_mismatch`, not silently
// overwrite the stored value.
func TestDeviceIDMismatch_DisablesFolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// First open seeds the original device_id.
	db, err := openFolderDB(dir, "ORIGINAL01")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	_ = db.Close()

	// Second open with a different identity must fail the device-id
	// check.
	db2, err := openFolderDB(dir, "ROTATED002")
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	err = checkDeviceID(db2, "ROTATED002")
	if err == nil {
		t.Fatal("expected errDeviceIDMismatch, got nil")
	}
	if !errors.Is(err, errDeviceIDMismatch) {
		t.Fatalf("expected errDeviceIDMismatch wrap, got %v", err)
	}
	// Error message must name both values so the operator can decide
	// which one to restore.
	if !strings.Contains(err.Error(), "ORIGINAL01") || !strings.Contains(err.Error(), "ROTATED002") {
		t.Errorf("error does not name both device IDs: %v", err)
	}
}

// TestIntegrityCheck_QuickSyncFullAsync pins H15 (audit §2.2 R2):
// quick_check runs synchronously at folder open and is fast (returns
// "ok" on a fresh DB); integrity_check is the deeper async pass.
// The test asserts both functions return nil on a fresh database
// (the happy path) and that quick_check is fast enough to run
// inline (under 1s on a fresh DB; this is well below the audit's
// "few ms" claim but provides headroom for slow CI).
func TestIntegrityCheck_QuickSyncFullAsync(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	start := time.Now()
	if err := runQuickCheck(db); err != nil {
		t.Fatalf("runQuickCheck on fresh DB: %v", err)
	}
	if d := time.Since(start); d > 1*time.Second {
		t.Errorf("quick_check too slow on fresh DB: %v (want < 1s)", d)
	}

	if err := runIntegrityCheck(context.Background(), db); err != nil {
		t.Fatalf("runIntegrityCheck on fresh DB: %v", err)
	}
}

// TestRunQuickCheck_DetectsCorruption pins the rejection path: a
// truncated SQLite file (overwritten with garbage at the page level)
// must trip quick_check. This is the failure scenario that promotes
// the folder to FolderDisabled(quick_check_failed).
func TestRunQuickCheck_DetectsCorruption(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		t.Fatalf("openFolderDB: %v", err)
	}
	// Close the DB so we can corrupt the file directly.
	_ = db.Close()

	// Overwrite the database file with garbage. Use a 4 KB write so
	// the SQLite page header (first 100 bytes) is mangled — this is
	// the case quick_check is designed to catch.
	dbPath := filepath.Join(dir, folderDBFilename)
	garbage := make([]byte, 4096)
	for i := range garbage {
		garbage[i] = byte(i % 251) // visibly non-zero, non-SQLite-magic
	}
	if err := os.WriteFile(dbPath, garbage, 0o600); err != nil {
		t.Fatalf("corrupt DB file: %v", err)
	}

	// Reopen; the open itself may succeed (modernc accepts the file
	// as a database file) but quick_check should reject it.
	db2, err := openFolderDB(dir, "ABCDE12345")
	if err != nil {
		// Open itself rejected the file — also an acceptable outcome
		// for the disable-on-corrupt path; the audit allows the
		// rejection at any step from open to quick_check.
		return
	}
	t.Cleanup(func() { _ = db2.Close() })

	err = runQuickCheck(db2)
	if err == nil {
		t.Fatal("quick_check on corrupt DB: want error, got nil")
	}
	if !errors.Is(err, errQuickCheckFailed) {
		t.Errorf("expected errQuickCheckFailed wrap, got %v", err)
	}
}

// TestDisable_RecordsTxRolledBack pins iter-4 Z6 / decision §5 #25:
// when disable() fires while a writer transaction is in flight, the
// resulting DisabledState records TxRolledBack=true so the operator
// sees that the in-flight tx was canceled.
//
// We simulate the in-flight state by setting txInFlight directly,
// then calling disable(); the assertion is on the recorded DisabledState.
func TestDisable_RecordsTxRolledBack(t *testing.T) {
	t.Parallel()
	fs := &folderState{}
	// Simulate an in-flight tx; the writerCancel hook is also wired
	// so disable() exercises the cancel path.
	_, cancel := context.WithCancel(context.Background())
	fs.writerCancel = cancel
	fs.txInFlight.Store(1)

	fs.disable(DisabledIntegrityCheck, "simulated integrity_check failure", "")
	ds := fs.DisabledStateSnapshot()
	if ds == nil {
		t.Fatal("DisabledStateSnapshot returned nil after disable()")
	}
	if !ds.TxRolledBack {
		t.Error("DisabledState.TxRolledBack=false; want true (txInFlight was 1 at disable() time)")
	}
	if ds.Reason != DisabledIntegrityCheck {
		t.Errorf("Reason=%q want %q", ds.Reason, DisabledIntegrityCheck)
	}
	if ds.Action == "" {
		t.Error("Action string is empty; disabledReasonActions map should populate it")
	}
}

// TestDisable_Idempotent pins that a second disable() call after the
// first is a no-op. The first reason wins; subsequent calls do not
// overwrite or churn the disabled state.
func TestDisable_Idempotent(t *testing.T) {
	t.Parallel()
	fs := &folderState{}

	fs.disable(DisabledQuickCheck, "first failure", "")
	first := fs.DisabledStateSnapshot()
	if first == nil {
		t.Fatal("first disable did not record state")
	}

	fs.disable(DisabledIntegrityCheck, "second failure", "")
	second := fs.DisabledStateSnapshot()
	if second.Reason != DisabledQuickCheck {
		t.Errorf("second disable() overwrote reason: got %q want %q",
			second.Reason, DisabledQuickCheck)
	}
	if second.ErrorText != "first failure" {
		t.Errorf("ErrorText changed after second disable: %q", second.ErrorText)
	}
}

// TestDisable_UnknownCapturesStackTrace pins iter-4 O8 (decision §5
// #19): when reason is `unknown`, the disabled state records a
// stack trace excerpt so the operator can escalate without a
// separate log query.
func TestDisable_UnknownCapturesStackTrace(t *testing.T) {
	t.Parallel()
	fs := &folderState{}
	fs.disable(DisabledUnknown, "mystery failure", "")
	ds := fs.DisabledStateSnapshot()
	if ds == nil {
		t.Fatal("DisabledStateSnapshot returned nil")
	}
	if ds.StackTrace == "" {
		t.Error("StackTrace empty for reason=unknown; iter-4 O8 requires inline diagnostic load")
	}
	// Must mention the disable() frame somewhere in the trace.
	if !strings.Contains(ds.StackTrace, "disable") {
		t.Errorf("StackTrace missing disable() frame:\n%s", ds.StackTrace)
	}
}
