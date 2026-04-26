// This wrapper implements driver.Driver / Conn / Stmt / Tx by
// forwarding to modernc.org/sqlite. The legacy driver.Conn.Begin
// and driver.Stmt.Exec / Query methods are intentionally
// implemented to preserve the full interface surface — newer
// callers route through the Context variants below, but the
// wrapper must accept either path because database/sql may pick
// the legacy method when the inner driver does not implement
// the Context variant. The deprecation warnings on those calls
// are unavoidable side effects of the wrapper pattern.
//lint:file-ignore SA1019 wrapping a database/sql/driver requires implementing the deprecated methods to preserve interface forwarding

package filesync

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"
	"sync/atomic"

	sqlite "modernc.org/sqlite"
)

// faultPoint names a controlled injection site inside the
// SQLite-backed write path. Audit §6 commit 10 / decision §5
// #13: re-run the H-series tests against a fault-injecting
// driver to exercise the SQL error paths that pure unit tests
// can't reach.
//
// The `_test.go` suffix keeps the wrapper out of the release
// binary; production code never sees the faulty driver.
type faultPoint string

const (
	faultPointBegin     faultPoint = "begin"
	faultPointCommit    faultPoint = "commit"
	faultPointPrepare   faultPoint = "prepare"
	faultPointStmtExec  faultPoint = "stmt_exec"
	faultPointConnExec  faultPoint = "conn_exec"
	faultPointQueryExec faultPoint = "conn_query"
)

// faultRule is one injection: at faultPoint `at`, return `err`
// the first `count` times, then revert. Zero-value count means
// "fire indefinitely until cleared." A query-shape filter
// (substring match against the SQL text) lets a test target a
// specific query without affecting unrelated calls.
type faultRule struct {
	at         faultPoint
	queryShape string // substring match; empty matches all
	err        error
	remaining  atomic.Int64 // -1 = unlimited, otherwise countdown
}

// faultRegistry holds all installed rules across the test
// process. Rules are matched in registration order; the first
// matching rule consumes its budget (if bounded) and fires.
//
// Tests install rules via installFault() and clear them via the
// returned cleanup func; t.Cleanup ensures the rules don't leak
// across tests.
type faultRegistry struct {
	mu    sync.Mutex
	rules []*faultRule
}

var faults = &faultRegistry{}

// installFault registers a fault at the given point. Returns a
// cleanup that removes the rule (idempotent).
//
// count <= 0 means "unlimited until cleanup."
func installFault(at faultPoint, queryShape string, err error, count int) (cleanup func()) {
	r := &faultRule{at: at, queryShape: queryShape, err: err}
	if count > 0 {
		r.remaining.Store(int64(count))
	} else {
		r.remaining.Store(-1)
	}
	faults.mu.Lock()
	faults.rules = append(faults.rules, r)
	faults.mu.Unlock()
	return func() {
		faults.mu.Lock()
		defer faults.mu.Unlock()
		for i, x := range faults.rules {
			if x == r {
				faults.rules = append(faults.rules[:i], faults.rules[i+1:]...)
				return
			}
		}
	}
}

// checkFault returns the injected error for the (point, query)
// pair, or nil if no rule matches. Decrements the rule's
// remaining budget atomically; rules with remaining=0 stop
// firing without being removed (cleanup removes them).
func checkFault(at faultPoint, query string) error {
	faults.mu.Lock()
	defer faults.mu.Unlock()
	for _, r := range faults.rules {
		if r.at != at {
			continue
		}
		if r.queryShape != "" && !contains(query, r.queryShape) {
			continue
		}
		// Bounded countdown.
		rem := r.remaining.Load()
		if rem == 0 {
			continue
		}
		if rem > 0 {
			if !r.remaining.CompareAndSwap(rem, rem-1) {
				continue
			}
		}
		return r.err
	}
	return nil
}

func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	if len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// faultyDriverName is the registered driver name for the
// fault-injecting wrapper. openFolderDB respects
// folderDBDriverOverride at the package level (set in TestMain
// or per-test) so production runs unchanged with "sqlite".
const faultyDriverName = "sqlite_faulty"

// init registers the faulty driver. Runs in the test binary
// only because the file is `_test.go`-suffixed.
func init() {
	sql.Register(faultyDriverName, &faultyDriver{inner: &sqlite.Driver{}})
}

// faultyDriver wraps modernc.org/sqlite's driver and forwards
// every call to the inner driver, with checkFault hooks at the
// audit-named injection points.
type faultyDriver struct {
	inner *sqlite.Driver
}

func (d *faultyDriver) Open(dsn string) (driver.Conn, error) {
	c, err := d.inner.Open(dsn)
	if err != nil {
		return nil, err
	}
	return &faultyConn{inner: c}, nil
}

// faultyConn wraps a driver.Conn. Forwards Prepare / Close /
// Begin / BeginTx / ExecContext / QueryContext with checkFault
// hooks at each.
type faultyConn struct {
	inner driver.Conn
}

func (c *faultyConn) Prepare(q string) (driver.Stmt, error) {
	if err := checkFault(faultPointPrepare, q); err != nil {
		return nil, err
	}
	st, err := c.inner.Prepare(q)
	if err != nil {
		return nil, err
	}
	return &faultyStmt{inner: st, query: q}, nil
}

func (c *faultyConn) Close() error { return c.inner.Close() }

func (c *faultyConn) Begin() (driver.Tx, error) {
	if err := checkFault(faultPointBegin, ""); err != nil {
		return nil, err
	}
	tx, err := c.inner.Begin() //nolint:staticcheck // SA1019: matches the wrapped interface
	if err != nil {
		return nil, err
	}
	return &faultyTx{inner: tx}, nil
}

func (c *faultyConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := checkFault(faultPointBegin, ""); err != nil {
		return nil, err
	}
	if cb, ok := c.inner.(driver.ConnBeginTx); ok {
		tx, err := cb.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		return &faultyTx{inner: tx}, nil
	}
	tx, err := c.inner.Begin() //nolint:staticcheck // SA1019: legacy driver fallback
	if err != nil {
		return nil, err
	}
	return &faultyTx{inner: tx}, nil
}

func (c *faultyConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if err := checkFault(faultPointConnExec, q); err != nil {
		return nil, err
	}
	if ce, ok := c.inner.(driver.ExecerContext); ok {
		return ce.ExecContext(ctx, q, args)
	}
	return nil, driver.ErrSkip
}

func (c *faultyConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if err := checkFault(faultPointQueryExec, q); err != nil {
		return nil, err
	}
	if cq, ok := c.inner.(driver.QueryerContext); ok {
		return cq.QueryContext(ctx, q, args)
	}
	return nil, driver.ErrSkip
}

// faultyStmt wraps driver.Stmt. The query the prepare returned
// is captured at construction so the stmt-exec / stmt-query
// hooks have a query string to filter on.
type faultyStmt struct {
	inner driver.Stmt
	query string
}

func (s *faultyStmt) Close() error  { return s.inner.Close() }
func (s *faultyStmt) NumInput() int { return s.inner.NumInput() }
func (s *faultyStmt) Exec(args []driver.Value) (driver.Result, error) { //nolint:staticcheck // SA1019: legacy driver path
	if err := checkFault(faultPointStmtExec, s.query); err != nil {
		return nil, err
	}
	return s.inner.Exec(args) //nolint:staticcheck // SA1019: legacy driver path
}
func (s *faultyStmt) Query(args []driver.Value) (driver.Rows, error) { //nolint:staticcheck // SA1019: legacy driver path
	return s.inner.Query(args) //nolint:staticcheck // SA1019: legacy driver path
}

// ExecContext is the modern driver-side stmt exec; ConnContext
// dispatches via this when the inner stmt supports it. We keep
// the hook on faultPointStmtExec so tests target stmt-level
// failures regardless of which path the SQL driver took.
func (s *faultyStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if err := checkFault(faultPointStmtExec, s.query); err != nil {
		return nil, err
	}
	if e, ok := s.inner.(driver.StmtExecContext); ok {
		return e.ExecContext(ctx, args)
	}
	values := make([]driver.Value, len(args))
	for i, a := range args {
		values[i] = a.Value
	}
	return s.inner.Exec(values) //nolint:staticcheck // SA1019: legacy driver fallback
}

func (s *faultyStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if q, ok := s.inner.(driver.StmtQueryContext); ok {
		return q.QueryContext(ctx, args)
	}
	values := make([]driver.Value, len(args))
	for i, a := range args {
		values[i] = a.Value
	}
	return s.inner.Query(values) //nolint:staticcheck // SA1019: legacy driver fallback
}

// faultyTx wraps driver.Tx. The Commit hook is the load-bearing
// injection point — H12, H13 etc. inject SQLITE_FULL here.
type faultyTx struct {
	inner driver.Tx
}

func (t *faultyTx) Commit() error {
	if err := checkFault(faultPointCommit, ""); err != nil {
		// Best-effort rollback so the underlying conn returns
		// to a clean state even after we synthesize a fault.
		_ = t.inner.Rollback()
		return err
	}
	return t.inner.Commit()
}

func (t *faultyTx) Rollback() error { return t.inner.Rollback() }
