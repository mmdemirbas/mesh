package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestRecorder(t *testing.T, level string) *Recorder {
	t.Helper()
	dir := t.TempDir()
	cfg := GatewayCfg{
		Name: "gw-" + strings.ReplaceAll(t.Name(), "/", "_"),
		Log: LogCfg{
			Level:       level,
			Dir:         dir,
			MaxFileSize: "10MB",
			MaxAge:      "720h",
		},
	}
	rec, err := NewRecorder(cfg, silentLogger())
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	return rec
}

func readRows(t *testing.T, dir string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	sort.Strings(files)
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatalf("open %s: %v", f, err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			var row map[string]any
			if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
				t.Fatalf("parse row: %v", err)
			}
			rows = append(rows, row)
		}
		_ = fh.Close()
	}
	return rows
}

func TestRecorder_RunIDPresentAndStableWithinProcess(t *testing.T) {
	t.Parallel()
	rec := newTestRecorder(t, LogLevelMetadata)
	id := rec.Request(RequestMeta{Gateway: "gw", StartTime: time.Now()}, []byte("{}"))
	rec.Response(id, ResponseMeta{Status: 200, Outcome: OutcomeOK, StartTime: time.Now(), EndTime: time.Now()}, nil)
	_ = rec.Close()

	rows := readRows(t, rec.dir)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	run1, ok1 := rows[0]["run"].(string)
	run2, ok2 := rows[1]["run"].(string)
	if !ok1 || !ok2 {
		t.Fatalf("run id missing on req/resp: %v / %v", rows[0]["run"], rows[1]["run"])
	}
	if run1 == "" || run2 == "" {
		t.Errorf("run id is empty")
	}
	if run1 != run2 {
		t.Errorf("run id mismatch within recorder: %q vs %q", run1, run2)
	}
	if len(run1) < 4 {
		t.Errorf("run id %q is too short", run1)
	}
}

func TestRecorder_RunIDDiffersAcrossInstances(t *testing.T) {
	t.Parallel()
	rec1 := newTestRecorder(t, LogLevelMetadata)
	rec2 := newTestRecorder(t, LogLevelMetadata)
	if rec1.runID == rec2.runID {
		t.Errorf("run ids collide across recorders: %q", rec1.runID)
	}
}

func TestRecorder_OffReturnsNil(t *testing.T) {
	t.Parallel()
	cfg := GatewayCfg{Name: "gw", Log: LogCfg{Level: LogLevelOff}}
	rec, err := NewRecorder(cfg, silentLogger())
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	if rec != nil {
		t.Fatalf("expected nil recorder when level=off, got %+v", rec)
	}
	// Nil methods are safe.
	id := rec.Request(RequestMeta{Gateway: "gw"}, []byte("{}"))
	if id != 0 {
		t.Errorf("Request on nil recorder = %d, want 0", id)
	}
	rec.Response(id, ResponseMeta{}, nil)
	if err := rec.Close(); err != nil {
		t.Errorf("Close on nil recorder: %v", err)
	}
}

func TestRecorder_MetadataLevelOmitsBody(t *testing.T) {
	t.Parallel()
	rec := newTestRecorder(t, LogLevelMetadata)
	start := time.Now()
	id := rec.Request(RequestMeta{
		Gateway:   "gw-test",
		Direction: "a2a",
		Model:     "claude-opus-4-6",
		Stream:    false,
		Method:    "POST",
		Path:      "/v1/messages",
		Headers:   map[string][]string{"Content-Type": {"application/json"}},
		StartTime: start,
	}, []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"secret prompt"}]}`))
	rec.Response(id, ResponseMeta{
		Status:    200,
		Outcome:   OutcomeOK,
		Usage:     &Usage{InputTokens: 12, OutputTokens: 34},
		StartTime: start,
		EndTime:   start.Add(250 * time.Millisecond),
	}, []byte(`{"content":[{"text":"secret response"}]}`))
	_ = rec.Close()

	rows := readRows(t, rec.dir)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	req, resp := rows[0], rows[1]
	if req["t"] != "req" || resp["t"] != "resp" {
		t.Fatalf("row order: req.t=%v resp.t=%v", req["t"], resp["t"])
	}
	if _, ok := req["body"]; ok {
		t.Errorf("metadata level must not log request body, got %v", req["body"])
	}
	if _, ok := resp["body"]; ok {
		t.Errorf("metadata level must not log response body, got %v", resp["body"])
	}
	if req["id"] != resp["id"] {
		t.Errorf("id mismatch: %v vs %v", req["id"], resp["id"])
	}
	if req["model"] != "claude-opus-4-6" {
		t.Errorf("model = %v", req["model"])
	}
	if resp["elapsed_ms"].(float64) != 250 {
		t.Errorf("elapsed_ms = %v, want 250", resp["elapsed_ms"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 12 || usage["output_tokens"].(float64) != 34 {
		t.Errorf("usage = %v", usage)
	}
}

func TestRecorder_FullLevelRecordsBody(t *testing.T) {
	t.Parallel()
	rec := newTestRecorder(t, LogLevelFull)
	reqBody := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}]}`)
	respBody := []byte(`{"content":[{"text":"hello"}]}`)
	id := rec.Request(RequestMeta{Gateway: "gw", StartTime: time.Now()}, reqBody)
	rec.Response(id, ResponseMeta{Status: 200, Outcome: OutcomeOK, StartTime: time.Now()}, respBody)
	_ = rec.Close()

	rows := readRows(t, rec.dir)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	reqBodyRow, ok := rows[0]["body"].(map[string]any)
	if !ok {
		t.Fatalf("full level must embed parsed request body, got %T: %v", rows[0]["body"], rows[0]["body"])
	}
	if reqBodyRow["model"] != "claude-opus-4-6" {
		t.Errorf("request body model = %v", reqBodyRow["model"])
	}
	respBodyRow, ok := rows[1]["body"].(map[string]any)
	if !ok {
		t.Fatalf("full level must embed parsed response body, got %T", rows[1]["body"])
	}
	if _, ok := respBodyRow["content"]; !ok {
		t.Errorf("response body missing content: %v", respBodyRow)
	}
}

func TestRecorder_RedactsSensitiveHeaders(t *testing.T) {
	t.Parallel()
	rec := newTestRecorder(t, LogLevelFull)
	id := rec.Request(RequestMeta{
		Gateway: "gw",
		Headers: map[string][]string{
			"Authorization": {"Bearer sk-secret-xyz"},
			"X-Api-Key":     {"sk-ant-another-secret"},
			"Content-Type":  {"application/json"},
		},
		StartTime: time.Now(),
	}, []byte("{}"))
	rec.Response(id, ResponseMeta{Status: 200, Outcome: OutcomeOK, StartTime: time.Now()}, []byte("{}"))
	_ = rec.Close()

	raw, err := os.ReadFile(filepath.Join(rec.dir, findFile(t, rec.dir)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(raw)
	for _, secret := range []string{"sk-secret-xyz", "sk-ant-another-secret"} {
		if strings.Contains(s, secret) {
			t.Errorf("audit log contains secret %q; contents:\n%s", secret, s)
		}
	}
	if !strings.Contains(s, "[redacted]") {
		t.Errorf("expected [redacted] marker in audit log; contents:\n%s", s)
	}
	if !strings.Contains(s, "application/json") {
		t.Errorf("non-sensitive header missing; contents:\n%s", s)
	}
}

func TestRecorder_SizeBasedRollover(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := GatewayCfg{
		Name: "gw-roll",
		Log:  LogCfg{Level: LogLevelFull, Dir: dir, MaxFileSize: "2K", MaxAge: "720h"},
	}
	rec, err := NewRecorder(cfg, silentLogger())
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	payload := strings.Repeat("A", 500)
	for i := 0; i < 20; i++ {
		id := rec.Request(RequestMeta{Gateway: "gw-roll", StartTime: time.Now()}, []byte(payload))
		rec.Response(id, ResponseMeta{Status: 200, Outcome: OutcomeOK, StartTime: time.Now()}, []byte(payload))
	}
	_ = rec.Close()

	files, err := filepath.Glob(filepath.Join(rec.dir, "*.jsonl"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) < 2 {
		t.Errorf("expected rollover to produce multiple files, got %d: %v", len(files), files)
	}
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatalf("stat %s: %v", f, err)
		}
		// Single rows may exceed max_size (rollover is checked before write,
		// not mid-row), but nothing should be wildly out of bounds.
		if info.Size() > 10*1024 {
			t.Errorf("file %s is %d bytes, far above 2KB cap", f, info.Size())
		}
	}
}

func TestRecorder_CleansUpFilesOlderThanMaxAge(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Pre-create an "old" file with mtime 100h in the past.
	oldPath := filepath.Join(dir, "2025-01-01.jsonl")
	if err := os.WriteFile(oldPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	old := time.Now().Add(-100 * time.Hour)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	// And a "fresh" file with mtime 1h ago.
	freshPath := filepath.Join(dir, "fresh.jsonl")
	if err := os.WriteFile(freshPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fresh := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(freshPath, fresh, fresh); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Recorder's own dir is dir/gw-clean; we want cleanup to run on the
	// seeded dir, so drop files directly into the recorder's target path.
	cfg := GatewayCfg{
		Name: "gw-clean",
		Log:  LogCfg{Level: LogLevelFull, Dir: dir, MaxFileSize: "1MB", MaxAge: "72h"},
	}
	gwDir := filepath.Join(dir, "gw-clean")
	if err := os.MkdirAll(gwDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Move seeded files under gwDir with the same mtimes.
	if err := os.Rename(oldPath, filepath.Join(gwDir, filepath.Base(oldPath))); err != nil {
		t.Fatalf("rename old: %v", err)
	}
	if err := os.Rename(freshPath, filepath.Join(gwDir, filepath.Base(freshPath))); err != nil {
		t.Fatalf("rename fresh: %v", err)
	}
	_ = os.Chtimes(filepath.Join(gwDir, "2025-01-01.jsonl"), old, old)
	_ = os.Chtimes(filepath.Join(gwDir, "fresh.jsonl"), fresh, fresh)

	rec, err := NewRecorder(cfg, silentLogger())
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	if _, err := os.Stat(filepath.Join(gwDir, "2025-01-01.jsonl")); !os.IsNotExist(err) {
		t.Errorf("old file should have been deleted, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(gwDir, "fresh.jsonl")); err != nil {
		t.Errorf("fresh file should have survived, err=%v", err)
	}
}

func findFile(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			return e.Name()
		}
	}
	t.Fatalf("no jsonl file in %s", dir)
	return ""
}
