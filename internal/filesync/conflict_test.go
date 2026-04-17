package filesync

import (
	"strings"
	"testing"
)

func TestOriginalPathFromConflict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		wantOk  bool
		wantOut string
	}{
		{
			name:    "standard with extension",
			input:   "report.sync-conflict-20260406-143022-abc123.docx",
			wantOk:  true,
			wantOut: "report.docx",
		},
		{
			name:    "nested path",
			input:   "sub/data.sync-conflict-20260101-000000-def456.csv",
			wantOk:  true,
			wantOut: "sub/data.csv",
		},
		{
			name:    "no extension",
			input:   "Makefile.sync-conflict-20260406-143022-abc123",
			wantOk:  true,
			wantOut: "Makefile",
		},
		{
			name:    "collision counter suffix",
			input:   "report.sync-conflict-20260406-143022-abc123-2.docx",
			wantOk:  true,
			wantOut: "report.docx",
		},
		{
			name:    "random hex suffix",
			input:   "report.sync-conflict-20260406-143022-abc123-a1b2c3d4.docx",
			wantOk:  true,
			wantOut: "report.docx",
		},
		{
			name:    "dots in original name",
			input:   "my.config.file.sync-conflict-20260406-143022-abc123.yaml",
			wantOk:  true,
			wantOut: "my.config.file.yaml",
		},
		{
			name:    "not a conflict file",
			input:   "normal.txt",
			wantOk:  false,
			wantOut: "",
		},
		{
			name:    "partial match not valid",
			input:   "file.sync-conflict-badformat.txt",
			wantOk:  false,
			wantOut: "",
		},
		{
			name:    "deeply nested",
			input:   "a/b/c/file.sync-conflict-20260406-143022-xyz789.txt",
			wantOk:  true,
			wantOut: "a/b/c/file.txt",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := OriginalPathFromConflict(tc.input)
			if ok != tc.wantOk {
				t.Fatalf("OriginalPathFromConflict(%q) ok = %v, want %v", tc.input, ok, tc.wantOk)
			}
			if got != tc.wantOut {
				t.Errorf("OriginalPathFromConflict(%q) = %q, want %q", tc.input, got, tc.wantOut)
			}
		})
	}
}

func TestDiffLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		a, b      []string
		maxOut    int
		wantOps   string // concatenated Op first letters: "e"qual, "a"dd, "d"elete
		wantTrunc bool
	}{
		{
			name:    "both empty",
			a:       nil,
			b:       nil,
			maxOut:  100,
			wantOps: "",
		},
		{
			name:    "identical single line",
			a:       []string{"hello"},
			b:       []string{"hello"},
			maxOut:  100,
			wantOps: "e",
		},
		{
			name:    "identical multi line",
			a:       []string{"a", "b", "c"},
			b:       []string{"a", "b", "c"},
			maxOut:  100,
			wantOps: "eee",
		},
		{
			name:    "single insert at end",
			a:       []string{"a", "b"},
			b:       []string{"a", "b", "c"},
			maxOut:  100,
			wantOps: "eea",
		},
		{
			name:    "single delete at end",
			a:       []string{"a", "b", "c"},
			b:       []string{"a", "b"},
			maxOut:  100,
			wantOps: "eed",
		},
		{
			name:    "single change in middle",
			a:       []string{"a", "b", "c"},
			b:       []string{"a", "x", "c"},
			maxOut:  100,
			wantOps: "edae",
		},
		{
			name:    "all different",
			a:       []string{"a", "b"},
			b:       []string{"x", "y"},
			maxOut:  100,
			wantOps: "ddaa", // Myers produces bulk delete then bulk add
		},
		{
			name:    "a empty, b has lines",
			a:       nil,
			b:       []string{"x", "y"},
			maxOut:  100,
			wantOps: "aa",
		},
		{
			name:    "b empty, a has lines",
			a:       []string{"x", "y"},
			b:       nil,
			maxOut:  100,
			wantOps: "dd",
		},
		{
			name:      "truncated output",
			a:         []string{"a"},
			b:         []string{"x", "y", "z"},
			maxOut:    2,
			wantOps:   "da",
			wantTrunc: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lines, trunc := diffLines(tc.a, tc.b, tc.maxOut)
			var ops strings.Builder
			for _, l := range lines {
				switch l.Op {
				case "equal":
					ops.WriteByte('e')
				case "add":
					ops.WriteByte('a')
				case "delete":
					ops.WriteByte('d')
				default:
					t.Fatalf("unexpected op: %q", l.Op)
				}
			}
			if ops.String() != tc.wantOps {
				t.Errorf("ops = %q, want %q", ops.String(), tc.wantOps)
			}
			if trunc != tc.wantTrunc {
				t.Errorf("truncated = %v, want %v", trunc, tc.wantTrunc)
			}
		})
	}
}

func TestDiffLines_TextContent(t *testing.T) {
	t.Parallel()
	a := []string{"line1", "line2", "line3"}
	b := []string{"line1", "changed", "line3"}

	lines, _ := diffLines(a, b, 100)

	// Verify the actual text content is correct.
	want := []DiffLine{
		{Op: "equal", Text: "line1"},
		{Op: "delete", Text: "line2"},
		{Op: "add", Text: "changed"},
		{Op: "equal", Text: "line3"},
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d", len(lines), len(want))
	}
	for i, l := range lines {
		if l.Op != want[i].Op || l.Text != want[i].Text {
			t.Errorf("line[%d] = {%q, %q}, want {%q, %q}", i, l.Op, l.Text, want[i].Op, want[i].Text)
		}
	}
}

func TestComputeConflictDiff_TextFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "report.txt", "line1\nline2\nline3\n")
	writeFile(t, dir, "report.sync-conflict-20260406-143022-abc123.txt", "line1\nchanged\nline3\n")

	diff, err := ComputeConflictDiff(dir, "report.sync-conflict-20260406-143022-abc123.txt")
	if err != nil {
		t.Fatal(err)
	}
	if diff.OriginalPath != "report.txt" {
		t.Errorf("OriginalPath = %q, want %q", diff.OriginalPath, "report.txt")
	}
	if !diff.OriginalExists {
		t.Error("OriginalExists = false, want true")
	}
	if diff.IsBinary {
		t.Error("IsBinary = true, want false")
	}
	if len(diff.Lines) == 0 {
		t.Fatal("expected diff lines")
	}
	// Should have: equal(line1), delete(line2), add(changed), equal(line3)
	hasDelete := false
	hasAdd := false
	for _, l := range diff.Lines {
		if l.Op == "delete" && l.Text == "line2" {
			hasDelete = true
		}
		if l.Op == "add" && l.Text == "changed" {
			hasAdd = true
		}
	}
	if !hasDelete || !hasAdd {
		t.Errorf("expected delete(line2) and add(changed) in diff, got %v", diff.Lines)
	}
}

func TestComputeConflictDiff_BinaryFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "image.png", "header\x00binary data")
	writeFile(t, dir, "image.sync-conflict-20260406-143022-abc123.png", "header\x00different")

	diff, err := ComputeConflictDiff(dir, "image.sync-conflict-20260406-143022-abc123.png")
	if err != nil {
		t.Fatal(err)
	}
	if !diff.IsBinary {
		t.Error("IsBinary = false, want true")
	}
	if diff.Conflict.SHA256 == "" || diff.Original.SHA256 == "" {
		t.Error("expected SHA256 hashes for binary files")
	}
	if diff.Conflict.SHA256 == diff.Original.SHA256 {
		t.Error("SHA256 hashes should differ for different content")
	}
	if len(diff.Lines) != 0 {
		t.Error("expected no diff lines for binary files")
	}
}

func TestComputeConflictDiff_MissingOriginal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "report.sync-conflict-20260406-143022-abc123.txt", "orphaned conflict")

	diff, err := ComputeConflictDiff(dir, "report.sync-conflict-20260406-143022-abc123.txt")
	if err != nil {
		t.Fatal(err)
	}
	if diff.OriginalExists {
		t.Error("OriginalExists = true, want false")
	}
	if diff.Conflict.Size == 0 {
		t.Error("expected non-zero conflict file size")
	}
}

func TestComputeConflictDiff_PathTraversal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create a file outside the folder root via symlink.
	_, err := ComputeConflictDiff(dir, "../etc/passwd.sync-conflict-20260406-143022-abc123.txt")
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
}

func TestComputeConflictDiff_NotConflictFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "normal.txt", "content")

	_, err := ComputeConflictDiff(dir, "normal.txt")
	if err == nil {
		t.Fatal("expected error for non-conflict file")
	}
}

// writeFile is defined in filesync_test.go — shared test helper.
