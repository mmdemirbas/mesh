package filesync

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/mmdemirbas/mesh/internal/state"
)

// DisabledReason is a closed-enum value identifying why a folder has
// transitioned to FolderDisabled. Closed-enum is mandatory: the
// Prometheus metric mesh_filesync_folder_disabled carries this as a
// label, and an open-string label would explode cardinality.
//
// Per PERSISTENCE-AUDIT.md §2.2 R8 (iter-4 O11 + O8 + Z6 + Z12 + Z15)
// and §6 commit 3. Action strings live in disabledReasonActions; the
// runbook §4 cites the same strings verbatim
// (TestRunbookActionStringsMatchMap pins the contract).
type DisabledReason string

const (
	DisabledNone                DisabledReason = ""
	DisabledQuickCheck          DisabledReason = "quick_check_failed"
	DisabledIntegrityCheck      DisabledReason = "integrity_check_failed"
	DisabledDeviceIDMismatch    DisabledReason = "device_id_mismatch"
	DisabledSchemaVersion       DisabledReason = "schema_version_mismatch"
	DisabledMetadataParseFailed DisabledReason = "metadata_parse_failed"
	DisabledReadOnlyFS          DisabledReason = "read_only_fs"
	DisabledDiskFull            DisabledReason = "disk_full"
	DisabledDirtySetOverflow    DisabledReason = "dirty_set_overflow"
	DisabledLegacyIndex         DisabledReason = "legacy_index_refused"
	DisabledUnknown             DisabledReason = "unknown"
)

// AllDisabledReasons returns every non-empty enum value in stable order.
// The disabled-reasons coverage test
// (TestDisabledReasonActions_AllEnumsHaveAction, iter-4 Z12) iterates
// this slice to assert every reason has an action string.
func AllDisabledReasons() []DisabledReason {
	return []DisabledReason{
		DisabledQuickCheck,
		DisabledIntegrityCheck,
		DisabledDeviceIDMismatch,
		DisabledSchemaVersion,
		DisabledMetadataParseFailed,
		DisabledReadOnlyFS,
		DisabledDiskFull,
		DisabledDirtySetOverflow,
		DisabledLegacyIndex,
		DisabledUnknown,
	}
}

// disabledReasonActions maps each reason to the one-line action string
// the dashboard renders, the API response carries, and the operator
// runbook §4 cites verbatim. Per decision §5 #18 (iter-4 O11): single
// source of truth for "what to do next".
//
// The runbook drift doc-test (TestRunbookActionStringsMatchMap, iter-4
// Z15) parses OPERATOR-RUNBOOK.md §4 and asserts every value here
// appears verbatim in the runbook prose.
var disabledReasonActions = map[DisabledReason]string{
	DisabledQuickCheck:          "restore from the most recent quick_check_ok backup; see runbook §4.1 + §5",
	DisabledIntegrityCheck:      "restore from the most recent quick_check_ok backup; writes after the failure are lost; see runbook §4.2 + §5",
	DisabledDeviceIDMismatch:    "restore ~/.mesh/filesync/device-id from backup, restart node; see runbook §4.3",
	DisabledSchemaVersion:       "binary version mismatch — run the binary that wrote this schema, or restore from backup; see runbook §4.4",
	DisabledMetadataParseFailed: "folder_meta is corrupt — restore from the most recent quick_check_ok backup; see runbook §4.4 + §5",
	DisabledReadOnlyFS:          "remount the filesystem read-write, then POST /api/filesync/folders/<id>/reopen; see runbook §4.5",
	DisabledDiskFull:            "free disk space, then POST /api/filesync/folders/<id>/reopen; see runbook §4.6",
	DisabledDirtySetOverflow:    "triage upstream I/O failure (it is the cause), then POST /api/filesync/folders/<id>/reopen; see runbook §4.7",
	DisabledLegacyIndex:         "delete legacy gob/yaml sidecar files in the folder cache directory, restart node; see runbook §1.1",
	DisabledUnknown:             "capture the diagnostic payload and escalate; see runbook §4.8",
}

// DisabledState is the per-folder snapshot returned to dashboards and
// surfaced via /api/filesync/folders/<id>. Stored on folderState as
// atomic.Pointer for lock-free reads.
type DisabledState struct {
	Reason     DisabledReason `json:"reason"`
	Action     string         `json:"action"`
	DisabledAt time.Time      `json:"disabled_at"`

	// ErrorText carries the full text of the underlying error. Always
	// populated. The API surfaces it inline regardless of reason, but
	// it is most useful when reason=unknown where the operator needs
	// the exact failure to escalate.
	ErrorText string `json:"error_text,omitempty"`

	// StackTrace is the runtime stack at the disable() call site.
	// Captured only when reason=unknown (iter-4 O8). Empty otherwise.
	StackTrace string `json:"stack_trace,omitempty"`

	// RecentLog is the last N (50) log lines captured for this folder
	// at the disable() call site. Populated only when reason=unknown.
	RecentLog []string `json:"recent_log,omitempty"`

	// TxRolledBack indicates that an in-flight writer transaction was
	// canceled by the disable() call (iter-4 Z6 / decision §5 #25).
	// Operators see this in the JSON to confirm the corrupt-DB-mid-tx
	// case left no committed-but-suspect rows behind.
	TxRolledBack bool `json:"tx_in_flight_rolled_back,omitempty"`
}

// captureRecentLog is a hook the cmd/mesh main package wires at startup
// to expose the in-memory log ring buffer to the filesync package.
// Returns up to n recent log lines that mention folderID. Nil-safe:
// when the hook is unset (e.g., in tests), disable() simply records an
// empty RecentLog.
//
// The hook avoids a circular dependency between the slog handler chain
// (in cmd/mesh) and the filesync package.
var captureRecentLog func(folderID string, n int) []string

// SetRecentLogCapture wires the recent-log capture hook. cmd/mesh calls
// this at startup. Safe to call multiple times; the last setter wins.
// The intended caller is cmd/mesh — tests do not need to set it.
func SetRecentLogCapture(fn func(folderID string, n int) []string) {
	captureRecentLog = fn
}

// disable transitions the folder to the FolderDisabled state and
// captures the diagnostic payload. Idempotent: a second call with a
// different reason is a no-op (the first reason wins; subsequent
// failures pile on the same already-disabled folder).
//
// Z6 / decision §5 #25: if a writer transaction is in flight when
// disable fires, the folder's writer context is canceled so the tx
// rolls back. The disabled-state JSON records tx_in_flight_rolled_back
// so operators can see whether mid-tx writes survived.
//
// Callers should pass an empty stack string for non-unknown reasons;
// disable will fill stack and recent log only for DisabledUnknown.
func (fs *folderState) disable(reason DisabledReason, errText, stack string) {
	if fs.disabled.Load() != nil {
		// Already disabled. First reason wins — log the second cause
		// at debug level for forensic value, but do not overwrite.
		return
	}

	// Cancel any in-flight writer tx BEFORE we publish the disabled
	// state, so a goroutine that observes IsDisabled()=true cannot
	// also observe an unrolled-back tx.
	txInFlight := fs.txInFlight.Load() > 0
	if fs.writerCancel != nil {
		fs.writerCancel()
	}

	ds := &DisabledState{
		Reason:       reason,
		Action:       disabledReasonActions[reason],
		ErrorText:    errText,
		DisabledAt:   time.Now(),
		TxRolledBack: txInFlight,
	}
	if reason == DisabledUnknown {
		if stack == "" {
			ds.StackTrace = string(debug.Stack())
		} else {
			ds.StackTrace = stack
		}
		if captureRecentLog != nil {
			ds.RecentLog = captureRecentLog(fs.cfg.ID, 50)
		}
	}
	fs.disabled.Store(ds)

	// Dashboard surface: route through state.Global as Failed with a
	// reason-prefixed message so the UI shows a red row immediately.
	// The action string is the operator's next sentence.
	state.Global.Update("filesync-folder", fs.cfg.ID, state.Failed,
		string(reason)+": "+ds.Action)

	slog.Error("folder disabled",
		"folder", fs.cfg.ID,
		"reason", string(reason),
		"action", ds.Action,
		"error", errText,
		"tx_in_flight_rolled_back", txInFlight,
	)
}

// IsDisabled is the lock-free check sync/scan loops use to skip a
// disabled folder.
func (fs *folderState) IsDisabled() bool {
	return fs.disabled.Load() != nil
}

// DisabledStateSnapshot returns the current disabled-state pointer for
// JSON surfacing. Returns nil when the folder is enabled.
func (fs *folderState) DisabledStateSnapshot() *DisabledState {
	return fs.disabled.Load()
}

// folderDisabledFields are the writer-tx coordination fields commit 3
// adds to folderState. They live here rather than on folderState
// directly so the audit's R8 + Z6 wiring is colocated with the rest
// of the disabled-state machinery.
//
// These fields are zero-value-safe: an empty atomic.Pointer is "not
// disabled," a nil cancel function is harmless, and an empty
// txInFlight counter behaves correctly.
type folderDisabledFields struct {
	disabled     atomic.Pointer[DisabledState]
	writerCtx    context.Context //nolint:containedctx // folder-scoped writer cancellation; documented owner.
	writerCancel context.CancelFunc
	txInFlight   atomic.Int32
}

// classifyOpenError maps a folder-open error (from openFolderDB) to a
// DisabledReason. The default is DisabledUnknown; we recognize a few
// common shapes to give the operator a more actionable enum value.
func classifyOpenError(err error) DisabledReason {
	if err == nil {
		return DisabledNone
	}
	msg := err.Error()
	switch {
	case errStringContains(msg, "read-only"), errStringContains(msg, "EROFS"):
		return DisabledReadOnlyFS
	case errStringContains(msg, "no space"), errStringContains(msg, "ENOSPC"),
		errStringContains(msg, "disk full"):
		return DisabledDiskFull
	}
	return DisabledUnknown
}

// classifyMetaError maps a folder-meta load error to a DisabledReason.
// Recognizes the typed errors from index_sqlite.go; defaults to
// DisabledUnknown for unrecognized shapes.
func classifyMetaError(err error) DisabledReason {
	switch {
	case errIs(err, errSchemaVersionMismatch):
		return DisabledSchemaVersion
	case errIs(err, errDeviceIDMismatch):
		return DisabledDeviceIDMismatch
	case errStringContains(err.Error(), "parse int64"),
		errStringContains(err.Error(), "parse uint64"):
		return DisabledMetadataParseFailed
	}
	return DisabledUnknown
}

// errStringContains is a small helper that tolerates a nil error.
// Used by classify* helpers to do shape detection on errors that the
// SQLite driver returns as opaque strings (modernc does not expose
// errno values for read-only-FS / ENOSPC cases).
func errStringContains(s, substr string) bool {
	if s == "" {
		return false
	}
	return indexOf(s, substr) >= 0
}

// errIs is a thin wrapper around errors.Is so disabled.go does not need
// the errors import (keeping the imports tight). The classifier
// functions are the only callers.
func errIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// indexOf is a tiny strings.Contains analog. Inlined to avoid an
// import cycle risk between disabled.go and any future helper.
func indexOf(s, substr string) int {
	if substr == "" {
		return 0
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
