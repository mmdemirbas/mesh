package gateway

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"context"
	"time"
)

// TestRouter_MultiKeyResolution_PoolOrder pins A.1: APIKeyEnvs is
// resolved at NewRouter time into a key pool. The first env value
// also lands on the legacy APIKey field so existing dispatch sites
// keep working.
func TestRouter_MultiKeyResolution_PoolOrder(t *testing.T) {
	t.Setenv("A_KEY_PRIMARY", "primary-secret")
	t.Setenv("A_KEY_SECONDARY", "secondary-secret")
	t.Setenv("A_KEY_TERTIARY", "tertiary-secret")
	cfg := &GatewayCfg{
		Name: "multi",
		Upstream: []UpstreamCfg{
			{
				Name:           "openai-multi",
				Target:         "https://example.com/v1/chat/completions",
				API:            APIOpenAI,
				APIKeyEnvs:     []string{"A_KEY_PRIMARY", "A_KEY_SECONDARY", "A_KEY_TERTIARY"},
				RotationPolicy: "round_robin",
			},
		},
		Routing: []RoutingRule{{ClientModel: []string{"*"}, UpstreamName: "openai-multi"}},
	}
	router, err := NewRouter(cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	up := router.Upstream("openai-multi")
	if up == nil {
		t.Fatal("upstream not found")
	}
	if up.APIKey != "primary-secret" {
		t.Errorf("APIKey = %q, want primary-secret (first resolved)", up.APIKey)
	}
	if up.Keys.Len() != 3 {
		t.Errorf("Keys.Len = %d, want 3", up.Keys.Len())
	}
	snaps := up.Keys.Snapshot()
	wantEnvs := []string{"A_KEY_PRIMARY", "A_KEY_SECONDARY", "A_KEY_TERTIARY"}
	for i, want := range wantEnvs {
		if snaps[i].EnvVar != want {
			t.Errorf("Keys[%d].EnvVar = %q, want %q", i, snaps[i].EnvVar, want)
		}
	}
	if up.Keys.Policy.Name() != "round_robin" {
		t.Errorf("Policy = %q, want round_robin", up.Keys.Policy.Name())
	}
}

// TestRouter_SingleKeyBackcompat asserts the §0.5 example config
// continues to work: APIKeyEnv (single) resolves to a one-element
// pool whose policy is "single" regardless of how the operator
// might set rotation_policy elsewhere.
func TestRouter_SingleKeyBackcompat(t *testing.T) {
	t.Setenv("LEGACY_KEY", "old-secret")
	cfg := &GatewayCfg{
		Name: "legacy",
		Upstream: []UpstreamCfg{{
			Name:      "panshi",
			Target:    "http://panshi.example/v1/chat/completions",
			API:       APIOpenAI,
			APIKeyEnv: "LEGACY_KEY",
		}},
		Routing: []RoutingRule{{ClientModel: []string{"*"}, UpstreamName: "panshi"}},
	}
	router, err := NewRouter(cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	up := router.Upstream("panshi")
	if up.APIKey != "old-secret" {
		t.Errorf("APIKey = %q, want old-secret", up.APIKey)
	}
	if up.Keys.Len() != 1 {
		t.Errorf("Keys.Len = %d, want 1", up.Keys.Len())
	}
	if up.Keys.Policy.Name() != "single" {
		t.Errorf("Policy = %q, want 'single' for single-key pool", up.Keys.Policy.Name())
	}
	// Picking always returns the same key.
	for i := 0; i < 5; i++ {
		k := up.Keys.Pick(context.Background(), RequestContext{Now: time.Now()})
		if k == nil || k.Value != "old-secret" {
			t.Errorf("iter %d: pick = %+v, want value=old-secret", i, k)
		}
	}
}

// TestRouter_PassthroughZeroKeys asserts the no-key passthrough
// case (APIKeyEnv unset) produces a zero-length pool and an empty
// APIKey, preserving the existing "client supplies its own auth"
// path.
func TestRouter_PassthroughZeroKeys(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{
		Name: "passthrough",
		Upstream: []UpstreamCfg{{
			Name:   "claude",
			Target: "https://api.anthropic.com/v1/messages",
			API:    APIAnthropic,
		}},
		Routing: []RoutingRule{{ClientModel: []string{"*"}, UpstreamName: "claude"}},
	}
	router, err := NewRouter(cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	up := router.Upstream("claude")
	if up.APIKey != "" {
		t.Errorf("APIKey = %q, want empty", up.APIKey)
	}
	if up.Keys.Len() != 0 {
		t.Errorf("Keys.Len = %d, want 0", up.Keys.Len())
	}
}

// newTestRouter is a small helper for router-only unit tests. It takes a
// ready-made GatewayCfg (with Name, Upstream, Routing populated) and a
// *bytes.Buffer destination for the logger so tests can inspect fallback
// warnings without touching stderr.
func newTestRouter(t *testing.T, cfg *GatewayCfg, logBuf *bytes.Buffer) *Router {
	t.Helper()
	h := slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	router, err := NewRouter(cfg, slog.New(h))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return router
}

// TestRouter_DefaultUpstream_DeterministicFallbackWithoutCatchAll pins the
// §3.2 contract: with no "*" rule, DefaultUpstream must return the
// upstream named by rules[0] — not a random entry from the upstreams map
// as the previous implementation did. The test constructs 10 routers
// from the same cfg and checks every call picks the same upstream, which
// would fail under Go's randomized map iteration.
func TestRouter_DefaultUpstream_DeterministicFallbackWithoutCatchAll(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{
		Name: "det-gw",
		Upstream: []UpstreamCfg{
			{Name: "first", Target: "http://first.example", API: APIOpenAI},
			{Name: "second", Target: "http://second.example", API: APIOpenAI},
			{Name: "third", Target: "http://third.example", API: APIOpenAI},
		},
		Routing: []RoutingRule{
			{ClientModel: []string{"claude-*"}, UpstreamName: "first"},
			{ClientModel: []string{"gpt-*"}, UpstreamName: "second"},
			{ClientModel: []string{"glm-*"}, UpstreamName: "third"},
		},
	}

	for i := 0; i < 10; i++ {
		var buf bytes.Buffer
		r := newTestRouter(t, cfg, &buf)
		got := r.DefaultUpstream()
		if got == nil || got.Cfg.Name != "first" {
			name := "<nil>"
			if got != nil {
				name = got.Cfg.Name
			}
			t.Fatalf("iteration %d: DefaultUpstream = %q, want %q (rules[0])", i, name, "first")
		}
	}
}

// TestRouter_DefaultUpstream_WarnsOncePerRouter pins the "one-shot per
// gateway" semantics: the warning fires the first time the fallback runs
// on this Router, and not again on subsequent calls on the same Router.
// Separate Router instances (= separate gateways) each get their own
// first-time warning.
func TestRouter_DefaultUpstream_WarnsOncePerRouter(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{
		Name: "gw-warn",
		Upstream: []UpstreamCfg{
			{Name: "only", Target: "http://only.example", API: APIOpenAI},
		},
		Routing: []RoutingRule{
			{ClientModel: []string{"specific-model"}, UpstreamName: "only"},
		},
	}

	var buf bytes.Buffer
	r := newTestRouter(t, cfg, &buf)

	// First call fires the warning.
	if r.DefaultUpstream() == nil {
		t.Fatal("first DefaultUpstream = nil")
	}
	warns := strings.Count(buf.String(), "gateway has no '*' rule")
	if warns != 1 {
		t.Fatalf("first call produced %d warnings, want 1; log=%s", warns, buf.String())
	}
	if !strings.Contains(buf.String(), `gateway=gw-warn`) ||
		!strings.Contains(buf.String(), `fallback_upstream=only`) {
		t.Errorf("warning must name the gateway and fallback upstream; log=%s", buf.String())
	}

	// Subsequent calls on the SAME router are silent.
	for i := 0; i < 5; i++ {
		_ = r.DefaultUpstream()
	}
	warns = strings.Count(buf.String(), "gateway has no '*' rule")
	if warns != 1 {
		t.Errorf("after 5 more calls on same router: %d warnings, want 1", warns)
	}

	// A SEPARATE router (= separate gateway) gets its own first-time
	// warning. Three underspecified gateways → three warnings, not one.
	var buf2 bytes.Buffer
	r2 := newTestRouter(t, cfg, &buf2)
	_ = r2.DefaultUpstream()
	if strings.Count(buf2.String(), "gateway has no '*' rule") != 1 {
		t.Errorf("separate router did not get its own warning; log2=%s", buf2.String())
	}
}

// TestRouter_DefaultUpstream_CatchAllWinsSilent: when a "*" rule is
// present, DefaultUpstream returns its upstream and no warning is
// emitted.
func TestRouter_DefaultUpstream_CatchAllWinsSilent(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{
		Name: "gw-catchall",
		Upstream: []UpstreamCfg{
			{Name: "specific", Target: "http://sp.example", API: APIOpenAI},
			{Name: "default", Target: "http://def.example", API: APIOpenAI},
		},
		Routing: []RoutingRule{
			{ClientModel: []string{"claude-*"}, UpstreamName: "specific"},
			{ClientModel: []string{"*"}, UpstreamName: "default"},
		},
	}
	var buf bytes.Buffer
	r := newTestRouter(t, cfg, &buf)
	got := r.DefaultUpstream()
	if got == nil || got.Cfg.Name != "default" {
		t.Fatalf("DefaultUpstream = %v, want 'default' (the '*' rule's upstream)", got)
	}
	if strings.Contains(buf.String(), "has no '*' rule") {
		t.Errorf("warning fired despite '*' rule being present; log=%s", buf.String())
	}
}

// TestRouter_DefaultUpstream_NoRulesReturnsNil: a Router with no rules
// returns nil and emits no warning (the warning is specifically about
// missing '*'; absent-rules is a different failure mode handled by the
// caller).
func TestRouter_DefaultUpstream_NoRulesReturnsNil(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{
		Name: "gw-norules",
		Upstream: []UpstreamCfg{
			{Name: "only", Target: "http://only.example", API: APIOpenAI},
		},
		Routing: nil,
	}
	var buf bytes.Buffer
	r := newTestRouter(t, cfg, &buf)
	if got := r.DefaultUpstream(); got != nil {
		t.Fatalf("DefaultUpstream with no rules = %v, want nil", got)
	}
	if strings.Contains(buf.String(), "has no '*' rule") {
		t.Errorf("warning should not fire when there are no rules; log=%s", buf.String())
	}
}

// TestRouter_DefaultUpstream_RaceSafety exercises the sync.Once guard
// under concurrent access to ensure exactly one warning is emitted when
// many goroutines race on the first DefaultUpstream call.
func TestRouter_DefaultUpstream_RaceSafety(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{
		Name: "gw-race",
		Upstream: []UpstreamCfg{
			{Name: "u", Target: "http://u.example", API: APIOpenAI},
		},
		Routing: []RoutingRule{
			{ClientModel: []string{"m"}, UpstreamName: "u"},
		},
	}
	var buf bytes.Buffer
	r := newTestRouter(t, cfg, &buf)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.DefaultUpstream()
		}()
	}
	wg.Wait()

	warns := strings.Count(buf.String(), "gateway has no '*' rule")
	if warns != 1 {
		t.Fatalf("50 concurrent calls produced %d warnings, want exactly 1", warns)
	}
}
