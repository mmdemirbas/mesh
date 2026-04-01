package main

import (
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/mmdemirbas/mesh/internal/state"
)

// ansiEscape matches ANSI CSI escape sequences (colors, cursor movement, etc.).
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// buildAdminMux returns the HTTP handler for the local admin server.
// All endpoints are read-only and served on localhost only.
func buildAdminMux(ring *logRing) *http.ServeMux {
	mux := http.NewServeMux()

	// GET / — JSON state snapshot; kept for backward compat with the status command.
	// GET /api/state — same, versioned alias.
	stateHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state.Global.Snapshot())
	})
	mux.Handle("/", stateHandler)
	mux.Handle("/api/state", stateHandler)

	// GET /api/logs — recent log lines as a JSON string array, ANSI codes stripped.
	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		lines := ring.Lines()
		plain := make([]string, len(lines))
		for i, l := range lines {
			plain[i] = ansiEscape.ReplaceAllString(l, "")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(plain)
	})

	return mux
}
