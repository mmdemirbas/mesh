package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// auditFilter is the set of selectors accepted by GET /api/gateway/audit
// beyond the legacy `limit`. Empty fields are wildcards. Filters apply at the
// pair level: a request row matches when its session/model/turn attributes
// match, a response row matches when its status/outcome/usage match, and the
// pair is included when both halves pass (or one half is missing because the
// other side of the response lane was truncated).
type auditFilter struct {
	session   string
	model     string
	outcome   string
	since     time.Time
	until     time.Time
	minTokens int
}

// auditQueryRow is the minimal parsed view of a JSONL row used to evaluate a
// filter. Keeping the raw bytes alongside lets us return the unmodified JSON
// to the client without re-marshaling.
type auditQueryRow struct {
	raw          []byte
	t            string // "req" or "resp"
	id           uint64
	run          string
	ts           time.Time
	sessionID    string
	model        string
	status       int
	outcome      string
	inputTokens  int
	outputTokens int
}

func parseAuditRow(line []byte) (auditQueryRow, bool) {
	var minimal struct {
		T         string `json:"t"`
		ID        uint64 `json:"id"`
		Run       string `json:"run"`
		TS        string `json:"ts"`
		SessionID string `json:"session_id"`
		Model     string `json:"model"`
		Status    int    `json:"status"`
		Outcome   string `json:"outcome"`
		Usage     *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
		Summary *struct {
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"stream_summary"`
	}
	if err := json.Unmarshal(line, &minimal); err != nil {
		return auditQueryRow{}, false
	}
	row := auditQueryRow{
		raw:       append([]byte(nil), line...),
		t:         minimal.T,
		id:        minimal.ID,
		run:       minimal.Run,
		sessionID: minimal.SessionID,
		model:     minimal.Model,
		status:    minimal.Status,
		outcome:   minimal.Outcome,
	}
	if t, err := time.Parse(time.RFC3339Nano, minimal.TS); err == nil {
		row.ts = t
	}
	switch {
	case minimal.Usage != nil:
		row.inputTokens = minimal.Usage.InputTokens + minimal.Usage.CacheReadInputTokens + minimal.Usage.CacheCreationInputTokens
		row.outputTokens = minimal.Usage.OutputTokens
	case minimal.Summary != nil && minimal.Summary.Usage != nil:
		row.inputTokens = minimal.Summary.Usage.InputTokens
		row.outputTokens = minimal.Summary.Usage.OutputTokens
	}
	return row, true
}

// pairKey identifies a request/response pair within an audit log. The run
// component disambiguates ids across mesh process restarts.
type pairKey struct {
	id  uint64
	run string
}

// listJSONLByMTimeDesc returns *.jsonl entries in dir sorted newest first.
func listJSONLByMTimeDesc(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := entries[:0]
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		ai, err1 := out[i].Info()
		aj, err2 := out[j].Info()
		if err1 != nil || err2 != nil {
			return false
		}
		return ai.ModTime().After(aj.ModTime())
	})
	return out, nil
}

// queryAuditRows walks audit files in dir from newest to oldest and returns
// rows matching the filter, oldest-first within the returned window. The
// returned (file, fileSize) describe the newest file scanned; legacy clients
// that don't use filters still see the same fields populated.
//
// Memory bound: scans at most maxScan rows total before stopping, so a
// pathological filter on a huge log cannot OOM the admin server.
func queryAuditRows(dir string, f auditFilter, limit int) ([]json.RawMessage, string, int64, error) {
	const maxScan = 200_000
	files, err := listJSONLByMTimeDesc(dir)
	if err != nil {
		return nil, "", 0, err
	}
	if len(files) == 0 {
		return nil, "", 0, nil
	}
	newestName := files[0].Name()
	var newestSize int64
	if info, err := files[0].Info(); err == nil {
		newestSize = info.Size()
	}

	// Fast path: no filter and limit fits a single file — just tail it.
	if f.isEmpty() {
		rows, err := tailJSONL(filepath.Join(dir, newestName), limit)
		return rows, newestName, newestSize, err
	}

	// Filtered path: walk files newest→oldest, collect matching pairs.
	pairs := map[pairKey]*pairBuilder{}
	matched := []*pairBuilder{}
	scanned := 0

	for _, e := range files {
		if scanned >= maxScan || len(matched) >= limit {
			break
		}
		path := filepath.Join(dir, e.Name())
		if err := scanFile(path, func(line []byte) bool {
			scanned++
			row, ok := parseAuditRow(line)
			if !ok {
				return scanned < maxScan
			}
			key := pairKey{id: row.id, run: row.run}
			pb, exists := pairs[key]
			if !exists {
				pb = &pairBuilder{}
				pairs[key] = pb
			}
			switch row.t {
			case "req":
				pb.req = row
				pb.haveReq = true
			case "resp":
				pb.resp = row
				pb.haveResp = true
			}
			if pb.haveReq && pb.haveResp && !pb.matched {
				if matchesFilter(pb, f) {
					pb.matched = true
					matched = append(matched, pb)
				}
			}
			return scanned < maxScan && len(matched) < limit
		}); err != nil && !errors.Is(err, errStopScan) {
			return nil, newestName, newestSize, err
		}
	}

	// Some pairs may be matchable on req-only attributes even though the
	// response row was lost — emit the request alone in that case.
	for _, pb := range pairs {
		if pb.matched || !pb.haveReq || pb.haveResp {
			continue
		}
		if matchesReqFilter(&pb.req, f) {
			pb.matched = true
			matched = append(matched, pb)
		}
	}

	// Sort matched pairs by request timestamp descending so the newest pair
	// shows first in the UI's flat list.
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].req.ts.After(matched[j].req.ts)
	})
	if len(matched) > limit {
		matched = matched[:limit]
	}

	out := make([]json.RawMessage, 0, len(matched)*2)
	for _, pb := range matched {
		if pb.haveReq {
			out = append(out, pb.req.raw)
		}
		if pb.haveResp {
			out = append(out, pb.resp.raw)
		}
	}
	return out, newestName, newestSize, nil
}

type pairBuilder struct {
	req, resp         auditQueryRow
	haveReq, haveResp bool
	matched           bool
}

func (f auditFilter) isEmpty() bool {
	return f.session == "" && f.model == "" && f.outcome == "" &&
		f.since.IsZero() && f.until.IsZero() && f.minTokens == 0
}

func matchesFilter(pb *pairBuilder, f auditFilter) bool {
	if !matchesReqFilter(&pb.req, f) {
		return false
	}
	if f.outcome != "" && pb.resp.outcome != f.outcome {
		return false
	}
	if f.minTokens > 0 {
		if pb.resp.inputTokens+pb.resp.outputTokens < f.minTokens {
			return false
		}
	}
	return true
}

func matchesReqFilter(r *auditQueryRow, f auditFilter) bool {
	if f.session != "" && r.sessionID != f.session {
		return false
	}
	if f.model != "" && r.model != f.model {
		return false
	}
	if !f.since.IsZero() && r.ts.Before(f.since) {
		return false
	}
	if !f.until.IsZero() && r.ts.After(f.until) {
		return false
	}
	return true
}

// errStopScan is returned by scanFile's callback (via the bool return) and
// propagates as a sentinel so callers can distinguish early-stop from a real
// I/O error. Returning it from scanFile keeps the loop logic compact.
var errStopScan = errors.New("audit scan stopped")

// scanFile reads dir/name as JSONL and invokes fn for each non-empty line.
// The callback returns true to continue, false to stop early.
func scanFile(path string, fn func(line []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if !fn(line) {
			return errStopScan
		}
	}
	return sc.Err()
}

// tailJSONL returns the last `limit` non-empty lines of path as RawMessage
// slices. Used by the no-filter fast path of queryAuditRows.
func tailJSONL(path string, limit int) ([]json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	ring := make([]json.RawMessage, 0, limit)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		copyLine := make([]byte, len(line))
		copy(copyLine, line)
		if len(ring) < limit {
			ring = append(ring, copyLine)
			continue
		}
		copy(ring, ring[1:])
		ring[len(ring)-1] = copyLine
	}
	return ring, sc.Err()
}

// findAuditPair scans every audit file in dir for the request/response pair
// identified by (id, run). Returns the raw req and resp rows if found. Used
// by GET /api/gateway/audit/pair.
func findAuditPair(dir string, id uint64, run string) (req, resp json.RawMessage, err error) {
	files, err := listJSONLByMTimeDesc(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range files {
		path := filepath.Join(dir, e.Name())
		ferr := scanFile(path, func(line []byte) bool {
			row, ok := parseAuditRow(line)
			if !ok || row.id != id || row.run != run {
				return req == nil || resp == nil
			}
			switch row.t {
			case "req":
				if req == nil {
					req = append(json.RawMessage(nil), line...)
				}
			case "resp":
				if resp == nil {
					resp = append(json.RawMessage(nil), line...)
				}
			}
			return req == nil || resp == nil
		})
		if ferr != nil && !errors.Is(ferr, errStopScan) {
			return req, resp, ferr
		}
		if req != nil && resp != nil {
			break
		}
	}
	if req == nil && resp == nil {
		return nil, nil, fmt.Errorf("pair not found")
	}
	return req, resp, nil
}
