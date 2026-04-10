//go:build e2e

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

// DefaultImage is the image built by `task build:e2e-image`. Scenarios can
// override it through NodeOptions for stub services (e.g. stub-llm).
const DefaultImage = "mesh-e2e:local"

// ConfigPath is where every mesh node reads its YAML config inside the
// container. The path matches the default Dockerfile layout and the
// `-f` flag the harness passes to `mesh up`.
const ConfigPath = "/root/.mesh/conf/mesh.yaml"

// AdminPort is the mesh admin HTTP port inside each container. It is always
// bound to 127.0.0.1 per mesh's config validator, so the harness reaches it
// via `docker exec curl` rather than a host port mapping.
const AdminPort = 7777

// File is a pre-start file to copy into a node. Scenarios use this to seed
// SSH keys, known_hosts, sync folders, or any other fixture the mesh config
// references at startup.
type File struct {
	// Path inside the container.
	Path string
	// Content is the raw file bytes.
	Content []byte
	// Mode defaults to 0600 when zero.
	Mode int64
}

// NodeOptions drives StartNode. Alias is mandatory; everything else is
// optional with sensible defaults for the common mesh-up scenario.
type NodeOptions struct {
	// Image overrides DefaultImage. Useful for stub services.
	Image string
	// Alias is both the network alias on the bridge and the mesh node name
	// passed to `mesh up <alias>`. The config YAML must define a service
	// keyed by the same name.
	Alias string
	// Config is the YAML body written to ConfigPath before startup. When
	// empty, no config file is written and Cmd must be set explicitly.
	Config string
	// Files is an extra list of fixture files to drop into the container
	// before startup. Keys, SSH known_hosts, banners, and sync seeds go
	// here.
	Files []File
	// Env sets container environment variables.
	Env map[string]string
	// Cmd overrides the default `mesh -f <ConfigPath> up <Alias>`
	// entrypoint arguments. Stub containers set this to run their own
	// binary instead.
	Cmd []string
	// Entrypoint overrides the image entrypoint entirely. Leave empty to
	// keep the Dockerfile default.
	Entrypoint []string
	// WaitFor overrides the default readiness strategy. The default waits
	// for the mesh admin API to respond at http://127.0.0.1:7777/api/state.
	WaitFor wait.Strategy
	// SkipConfig disables the automatic config file write. Used by stub
	// containers that do not run mesh.
	SkipConfig bool
	// StartupTimeout bounds the readiness wait. Defaults to 60s.
	StartupTimeout time.Duration
}

// Node wraps a running mesh (or stub) container on a Network and exposes the
// handful of operations scenarios need: exec, file I/O, logs, and admin API.
type Node struct {
	t         testing.TB
	container testcontainers.Container
	Alias     string
	Network   *Network
}

// StartNode creates and starts a container on the given network with the
// supplied options. Failure is fatal via t.Fatalf so scenarios stay linear.
// Cleanup is registered with t.Cleanup.
func StartNode(ctx context.Context, t testing.TB, net *Network, opts NodeOptions) *Node {
	t.Helper()
	if opts.Alias == "" {
		t.Fatal("e2e: NodeOptions.Alias is required")
	}

	image := opts.Image
	if image == "" {
		image = DefaultImage
	}

	cmd := opts.Cmd
	if len(cmd) == 0 && !opts.SkipConfig {
		cmd = []string{"-f", ConfigPath, "up", opts.Alias}
	}

	timeout := opts.StartupTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	waitStrategy := opts.WaitFor
	if waitStrategy == nil && !opts.SkipConfig {
		// Poll the admin API until it responds. The inner curl uses
		// `--max-time 2` so one slow call never steals the whole timeout.
		waitStrategy = wait.ForExec([]string{
			"sh", "-c",
			fmt.Sprintf("curl -fs --max-time 2 http://127.0.0.1:%d/api/state >/dev/null", AdminPort),
		}).WithStartupTimeout(timeout).WithPollInterval(250 * time.Millisecond)
	}

	files := make([]testcontainers.ContainerFile, 0, len(opts.Files)+1)
	if !opts.SkipConfig && opts.Config != "" {
		files = append(files, testcontainers.ContainerFile{
			Reader:            strings.NewReader(opts.Config),
			ContainerFilePath: ConfigPath,
			FileMode:          0o600,
		})
	}
	for _, f := range opts.Files {
		mode := f.Mode
		if mode == 0 {
			mode = 0o600
		}
		files = append(files, testcontainers.ContainerFile{
			Reader:            bytes.NewReader(f.Content),
			ContainerFilePath: f.Path,
			FileMode:          mode,
		})
	}

	req := testcontainers.ContainerRequest{
		Image:          image,
		Name:           fmt.Sprintf("mesh-e2e-%s-%d", opts.Alias, time.Now().UnixNano()),
		Networks:       []string{net.Name},
		NetworkAliases: map[string][]string{net.Name: {opts.Alias}},
		Files:          files,
		Env:            opts.Env,
		Cmd:            cmd,
		Entrypoint:     opts.Entrypoint,
		WaitingFor:     waitStrategy,
	}

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("e2e: start node %q: %v", opts.Alias, err)
	}

	node := &Node{t: t, container: c, Alias: opts.Alias, Network: net}
	t.Cleanup(func() {
		// Use a detached context; the test's own context may already be
		// cancelled by the time Cleanup runs.
		termCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.Terminate(termCtx); err != nil {
			t.Logf("e2e: terminate %q: %v", opts.Alias, err)
		}
	})
	return node
}

// Container returns the raw testcontainers handle. Scenarios should prefer
// the typed helpers; this is an escape hatch.
func (n *Node) Container() testcontainers.Container {
	return n.container
}

// Exec runs a command inside the container and returns (exitCode, combined
// stdout+stderr, error). Never returns a non-nil error for a non-zero exit
// code — callers inspect the returned code.
func (n *Node) Exec(ctx context.Context, cmd ...string) (int, string, error) {
	code, reader, err := n.container.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		return code, "", fmt.Errorf("exec %v: %w", cmd, err)
	}
	var buf bytes.Buffer
	if reader != nil {
		if _, copyErr := io.Copy(&buf, reader); copyErr != nil && !errors.Is(copyErr, io.EOF) {
			return code, buf.String(), fmt.Errorf("read exec output: %w", copyErr)
		}
	}
	return code, buf.String(), nil
}

// MustExec runs Exec and fails the test on either a transport error or a
// non-zero exit code. Returns the combined output.
func (n *Node) MustExec(ctx context.Context, cmd ...string) string {
	n.t.Helper()
	code, out, err := n.Exec(ctx, cmd...)
	if err != nil {
		n.t.Fatalf("e2e: %s: exec %v: %v", n.Alias, cmd, err)
	}
	if code != 0 {
		n.t.Fatalf("e2e: %s: exec %v exit=%d output=%q", n.Alias, cmd, code, out)
	}
	return out
}

// WriteFile copies content into the container at path with the given mode
// (0600 when zero).
func (n *Node) WriteFile(ctx context.Context, path string, content []byte, mode int64) error {
	if mode == 0 {
		mode = 0o600
	}
	return n.container.CopyToContainer(ctx, content, path, mode)
}

// ReadFile reads a file from inside the container. Returns an empty slice
// and an error when the file is missing or unreadable.
func (n *Node) ReadFile(ctx context.Context, path string) ([]byte, error) {
	rc, err := n.container.CopyFileFromContainer(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("copy file %s: %w", path, err)
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// Logs returns the full docker log stream for the container. Scenarios use
// this for diagnostics; mesh's own log file under /root/.mesh/log is a
// separate source available via ReadFile.
func (n *Node) Logs(ctx context.Context) ([]byte, error) {
	rc, err := n.container.Logs(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// AdminGET executes a curl against the container's admin API and returns
// the raw body. Path must include the leading slash (e.g. "/api/state").
func (n *Node) AdminGET(ctx context.Context, path string) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", AdminPort, path)
	code, out, err := n.Exec(ctx, "curl", "-fsS", "--max-time", "5", url)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("admin GET %s: exit=%d body=%q", path, code, out)
	}
	return out, nil
}

// AdminJSON executes an admin GET and decodes the response body into v.
func (n *Node) AdminJSON(ctx context.Context, path string, v any) error {
	body, err := n.AdminGET(ctx, path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// StateSnapshot mirrors the JSON shape returned by GET /api/state. Only the
// fields scenarios assert on are typed; everything else is preserved as
// json.RawMessage so new state fields do not break tests.
type StateSnapshot struct {
	Components map[string]ComponentState `json:"components"`
}

// ComponentState is one entry inside StateSnapshot.Components.
type ComponentState struct {
	Type        string    `json:"type"`
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	Detail      string    `json:"detail,omitempty"`
	LastUpdated time.Time `json:"last_updated,omitempty"`
}

// AdminState fetches /api/state and returns the decoded snapshot.
func (n *Node) AdminState(ctx context.Context) (StateSnapshot, error) {
	// The mesh admin API currently returns the raw state map at the top
	// level. Decode into a map keyed by component key first, then wrap in
	// StateSnapshot so the rest of the harness can stay stable.
	var raw map[string]ComponentState
	if err := n.AdminJSON(ctx, "/api/state", &raw); err != nil {
		return StateSnapshot{}, err
	}
	return StateSnapshot{Components: raw}, nil
}

// ComponentByType returns the first component in the snapshot with a
// matching type and id. Useful for targeted assertions without string
// matching on the internal component key format.
func (s StateSnapshot) ComponentByType(compType, id string) (ComponentState, bool) {
	for _, c := range s.Components {
		if c.Type == compType && c.ID == id {
			return c, true
		}
	}
	return ComponentState{}, false
}

// AdminMetrics fetches /api/metrics as raw Prometheus text.
func (n *Node) AdminMetrics(ctx context.Context) (string, error) {
	return n.AdminGET(ctx, "/api/metrics")
}

// MetricValue parses a single Prometheus text line of the form
//
//	name{label="x"} 42
//
// and returns the numeric value. Matches the first line whose series name
// equals metric and whose label selector (as-is substring) matches sel.
// Returns (0, false) when no such line is present.
func MetricValue(body, metric, sel string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, metric) {
			continue
		}
		rest := strings.TrimPrefix(line, metric)
		if sel != "" && !strings.Contains(rest, sel) {
			continue
		}
		sp := strings.LastIndexByte(rest, ' ')
		if sp < 0 {
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(strings.TrimSpace(rest[sp+1:]), "%f", &v); err != nil {
			continue
		}
		return v, true
	}
	return 0, false
}

// Stop stops the container gracefully with the given timeout.
func (n *Node) Stop(ctx context.Context, timeout time.Duration) error {
	return n.container.Stop(ctx, &timeout)
}

// Start re-starts a previously stopped container.
func (n *Node) Start(ctx context.Context) error {
	return n.container.Start(ctx)
}
