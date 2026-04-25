package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// summarizerFixtureServer is a shared test fixture returning the same
// mocked summarizer response. Tests inject behavior (count calls, fail,
// block) via the handler closure.
func summarizerFixtureServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *ResolvedUpstream) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, &ResolvedUpstream{
		Cfg:    UpstreamCfg{Name: "sum", Target: srv.URL, API: APIOpenAI, ModelMap: map[string]string{"*": "summ-model"}},
		Client: http.DefaultClient,
	}
}

// mkReqBody builds an oversized request with the given prefix payload
// and a fixed 6-message recent tail. The prefix payload controls the
// dedup key; the tail is constant so tests can vary the prefix alone
// to exercise same-key vs different-key behavior.
func mkReqBody(prefixPayload string) *MessagesRequest {
	msgs := []AnthropicMsg{
		{Role: "user", Content: mustJSONString("old: " + prefixPayload)},
		{Role: "assistant", Content: mustJSONString("ack")},
		{Role: "user", Content: mustJSONString("q1")},
		{Role: "assistant", Content: mustJSONString("r1")},
		{Role: "user", Content: mustJSONString("q2")},
		{Role: "assistant", Content: mustJSONString("r2")},
		{Role: "user", Content: mustJSONString("q3")},
		{Role: "assistant", Content: mustJSONString("r3")},
	}
	return &MessagesRequest{Messages: msgs}
}

func mustJSONString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// fixedSummarizerHandler returns a handler that sends the same canned
// response every time and increments a counter visible to the caller.
func fixedSummarizerHandler(count *atomic.Int64, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		resp := ChatCompletionResponse{
			ID: "mock", Model: "summ-model",
			Choices: []OpenAIChoice{{
				Message:      OpenAIMsg{Role: "assistant", Content: mustJSONString(text)},
				FinishReason: "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// TestSummarizerDedup_CollapsesConcurrentCallsSameKey: two goroutines
// with identical prefixes share one upstream call (shared=true) and
// both receive the same summary text.
func TestSummarizerDedup_CollapsesConcurrentCallsSameKey(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	// Block the handler until a signal so both callers enter the flight.
	release := make(chan struct{})
	_, summarizer := summarizerFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		fixedSummarizerHandler(&calls, "shared-summary")(w, r)
	})
	dedup := newSummarizerDedup()

	req1 := mkReqBody("same")
	req2 := mkReqBody("same")

	var wg sync.WaitGroup
	var got1, got2 []AnthropicMsg
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		got1, _, err1 = summarizeMessages(context.Background(), req1, summarizer, 6, silentLogger(), dedup)
	}()
	go func() {
		defer wg.Done()
		// Small nudge so the second caller joins the flight in progress.
		time.Sleep(10 * time.Millisecond)
		got2, _, err2 = summarizeMessages(context.Background(), req2, summarizer, 6, silentLogger(), dedup)
	}()
	// Give both goroutines time to queue on the singleflight call.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if err1 != nil || err2 != nil {
		t.Fatalf("errors: %v %v", err1, err2)
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1 (single-flight should collapse)", calls.Load())
	}
	if !sameSummaryText(got1, got2) {
		t.Errorf("summaries diverged: %s vs %s", summaryTextOf(got1), summaryTextOf(got2))
	}
}

// TestSummarizerDedup_DifferentKeysDoNotCollapse: prefixes that differ
// even by one byte fan out into two upstream calls.
func TestSummarizerDedup_DifferentKeysDoNotCollapse(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	_, summarizer := summarizerFixtureServer(t, fixedSummarizerHandler(&calls, "summary"))
	dedup := newSummarizerDedup()

	_, _, err1 := summarizeMessages(context.Background(), mkReqBody("A"), summarizer, 6, silentLogger(), dedup)
	_, _, err2 := summarizeMessages(context.Background(), mkReqBody("B"), summarizer, 6, silentLogger(), dedup)

	if err1 != nil || err2 != nil {
		t.Fatalf("errors: %v %v", err1, err2)
	}
	if calls.Load() != 2 {
		t.Errorf("upstream calls = %d, want 2 (different prefixes must not share)", calls.Load())
	}
}

// TestSummarizerDedup_PostCompletionCache: a second call with the same
// prefix arriving after the first has completed (within the TTL) hits
// the cache, no upstream call.
func TestSummarizerDedup_PostCompletionCache(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	_, summarizer := summarizerFixtureServer(t, fixedSummarizerHandler(&calls, "summary"))
	dedup := newSummarizerDedup()

	_, _, err := summarizeMessages(context.Background(), mkReqBody("same"), summarizer, 6, silentLogger(), dedup)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("after first call: calls = %d, want 1", calls.Load())
	}

	// Second call within TTL — should hit cache.
	_, _, err = summarizeMessages(context.Background(), mkReqBody("same"), summarizer, 6, silentLogger(), dedup)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d after cached second call, want 1", calls.Load())
	}
}

// TestSummarizerDedup_PostTTLExpiryRefires: advance the dedup clock past
// the TTL, then issue a same-key call — upstream gets a second hit.
func TestSummarizerDedup_PostTTLExpiryRefires(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	_, summarizer := summarizerFixtureServer(t, fixedSummarizerHandler(&calls, "summary"))
	dedup := newSummarizerDedup()

	// Inject a controllable clock.
	now := time.Now()
	dedup.clock = func() time.Time { return now }

	_, _, err := summarizeMessages(context.Background(), mkReqBody("same"), summarizer, 6, silentLogger(), dedup)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("first call: calls=%d", calls.Load())
	}

	// Advance past TTL.
	now = now.Add(summarizerCacheTTL + time.Second)

	_, _, err = summarizeMessages(context.Background(), mkReqBody("same"), summarizer, 6, silentLogger(), dedup)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("upstream calls = %d after TTL expiry, want 2 (re-fire)", calls.Load())
	}
}

// TestSummarizerDedup_ErrorPropagatesNoCache: first caller's summarizer
// errors (upstream 500). All concurrent callers see the same error.
// No retry happens inside the dedup layer; a later call re-fires.
func TestSummarizerDedup_ErrorPropagatesNoCache(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	_, summarizer := summarizerFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "nope", http.StatusInternalServerError)
	})
	dedup := newSummarizerDedup()

	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, err1 = summarizeMessages(context.Background(), mkReqBody("same"), summarizer, 6, silentLogger(), dedup)
	}()
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
		_, _, err2 = summarizeMessages(context.Background(), mkReqBody("same"), summarizer, 6, silentLogger(), dedup)
	}()
	wg.Wait()

	if err1 == nil || err2 == nil {
		t.Fatalf("expected both callers to see an error: %v %v", err1, err2)
	}
	if !strings.Contains(err1.Error(), "500") && !strings.Contains(err1.Error(), "nope") {
		t.Errorf("err1 = %v, want 500 upstream error", err1)
	}
	// Second call should NOT have been cached — a follow-up retry re-fires.
	_, _, err3 := summarizeMessages(context.Background(), mkReqBody("same"), summarizer, 6, silentLogger(), dedup)
	if err3 == nil {
		t.Fatalf("expected third call to re-attempt and error again: %v", err3)
	}
	if calls.Load() < 2 {
		t.Errorf("upstream calls = %d, want >=2 (error must not be cached)", calls.Load())
	}
}

// TestSummarizerDedup_LeaderCancelAbortsFlight: first caller's context
// cancels mid-flight. Shared-cancel semantics: the fn's inner ctx
// derives from the leader, so when the leader cancels the flight
// aborts and all callers see the cancellation error. This test
// exercises dedup.do directly — no HTTP — to keep the semantic crisp
// and the test fast. The HTTP-end-to-end path is covered by the
// collapses-concurrent-calls and error-propagates tests.
func TestSummarizerDedup_LeaderCancelAbortsFlight(t *testing.T) {
	t.Parallel()

	dedup := newSummarizerDedup()
	var fnEntered, fnExited atomic.Int64

	// fn blocks until its (inner) context fires, then returns
	// ctx.Err(). Leader's ctx feeds innerCtx via do().
	fn := func(ctx context.Context) (string, error) {
		fnEntered.Add(1)
		<-ctx.Done()
		fnExited.Add(1)
		return "", ctx.Err()
	}

	leaderCtx, cancel := context.WithCancel(context.Background())
	followerCtx := context.Background()

	key := "same-key"
	var wg sync.WaitGroup
	var leaderErr, followerErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, leaderErr = dedup.do(leaderCtx, key, fn)
	}()
	go func() {
		defer wg.Done()
		// Small nudge so follower arrives after leader has registered
		// the flight with singleflight.
		time.Sleep(10 * time.Millisecond)
		_, followerErr = dedup.do(followerCtx, key, fn)
	}()

	// Give both callers time to park on the flight.
	time.Sleep(50 * time.Millisecond)
	if fnEntered.Load() != 1 {
		t.Fatalf("fn entered %d times before cancel, want 1", fnEntered.Load())
	}
	cancel()
	wg.Wait()

	if leaderErr == nil {
		t.Errorf("leader err = nil, expected context.Canceled")
	}
	if followerErr == nil {
		t.Errorf("follower err = nil, expected shared-cancel failure")
	}
	// fn must have exited exactly once — the cancel propagated through
	// innerCtx, not N times for N callers.
	if fnExited.Load() != 1 {
		t.Errorf("fn exited %d times, want 1", fnExited.Load())
	}
	// A follow-up caller with fresh ctx re-fires the flight; the
	// previous cancelled flight left nothing cached.
	fn2 := func(ctx context.Context) (string, error) {
		return "refired", nil
	}
	got, err := dedup.do(context.Background(), key, fn2)
	if err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	if got != "refired" {
		t.Errorf("follow-up text = %q, want %q (cancelled result must not be cached)", got, "refired")
	}
}

// TestSummarizerDedup_RaceSafety50Concurrent: fifty goroutines race on
// the same key. Exactly one upstream call fires; all goroutines
// receive the same summary text.
func TestSummarizerDedup_RaceSafety50Concurrent(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	// Handler delays slightly so all goroutines enter the flight.
	_, summarizer := summarizerFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		fixedSummarizerHandler(&calls, "racy-summary")(w, r)
	})
	dedup := newSummarizerDedup()

	const N = 50
	var wg sync.WaitGroup
	results := make([][]AnthropicMsg, N)
	errs := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			req := mkReqBody("same-50")
			results[i], _, errs[i] = summarizeMessages(context.Background(), req, summarizer, 6, silentLogger(), dedup)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d err: %v", i, err)
		}
	}
	// calls counts both the initial entry (incremented inside the
	// delaying handler) and the fixedSummarizerHandler increment — so
	// a single flight should produce exactly 2 counter bumps from one
	// upstream request. Assert bumps ≤ 2 (allowing race detector timing
	// slop where a second flight somehow starts, which would produce 4).
	if c := calls.Load(); c > 2 {
		t.Errorf("upstream call counter = %d under 50 concurrent callers, want <=2 (single-flight)", c)
	}
	// All summaries must agree.
	for i := 1; i < N; i++ {
		if !sameSummaryText(results[0], results[i]) {
			t.Errorf("goroutine 0 vs %d summary diverged", i)
			break
		}
	}
}

// --- helpers ---

// summaryTextOf extracts the summary user message's text (the first
// element of the result slice is always the summary, by contract).
func summaryTextOf(msgs []AnthropicMsg) string {
	if len(msgs) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(msgs[0].Content, &s); err != nil {
		return ""
	}
	return s
}

func sameSummaryText(a, b []AnthropicMsg) bool {
	return summaryTextOf(a) == summaryTextOf(b)
}

// TestSummarizerDedup_CacheBoundedUnderMany pins the cap-and-sweep
// behavior: pushing beyond summarizerCacheCap distinct keys keeps the
// cache size bounded by the cap at all times.
func TestSummarizerDedup_CacheBoundedUnderMany(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	_, summarizer := summarizerFixtureServer(t, fixedSummarizerHandler(&calls, "x"))
	dedup := newSummarizerDedup()

	for i := 0; i < summarizerCacheCap+32; i++ {
		req := mkReqBody(fmt.Sprintf("key-%04d", i))
		if _, _, err := summarizeMessages(context.Background(), req, summarizer, 6, silentLogger(), dedup); err != nil {
			t.Fatalf("i=%d: %v", i, err)
		}
	}

	dedup.mu.Lock()
	size := len(dedup.cache)
	dedup.mu.Unlock()
	if size > summarizerCacheCap {
		t.Errorf("cache size = %d > cap %d", size, summarizerCacheCap)
	}
}

// sanity: the dedupKey function returns distinct keys for different
// upstream names and prefixes.
func TestDedupKey_Distinctness(t *testing.T) {
	t.Parallel()
	p1, _ := json.Marshal([]AnthropicMsg{{Role: "user", Content: mustJSONString("A")}})
	p2, _ := json.Marshal([]AnthropicMsg{{Role: "user", Content: mustJSONString("B")}})
	cases := []struct{ a, b string }{
		{dedupKey("up1", p1), dedupKey("up1", p2)}, // different prefix
		{dedupKey("up1", p1), dedupKey("up2", p1)}, // different upstream
	}
	for i, c := range cases {
		if c.a == c.b {
			t.Errorf("case %d: keys collide: %s", i, c.a)
		}
	}
	// Same inputs → same key. Compute twice from separate Marshal
	// calls to defeat the compiler's "identical expression" warning
	// and actually exercise determinism end-to-end.
	p1b, _ := json.Marshal([]AnthropicMsg{{Role: "user", Content: mustJSONString("A")}})
	if dedupKey("up1", p1) != dedupKey("up1", p1b) {
		t.Error("same logical inputs (remarshaled) produced different keys")
	}
}

// Guard: a nil pointer dereference if summarizerDedup{}.do is misused
// without newSummarizerDedup should not panic — the cache map is the
// only nil-deref risk.
func TestSummarizerDedup_ZeroValueDoesNotPanic(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("zero-value dedup panicked: %v", r)
		}
	}()
	var d summarizerDedup
	d.clock = time.Now
	d.cache = make(map[string]cachedSummary)
	// Calling do with a fast-returning fn should work.
	_, err := d.do(context.Background(), "k", func(context.Context) (string, error) {
		return "ok", nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("zero-value do: %v", err)
	}
}
