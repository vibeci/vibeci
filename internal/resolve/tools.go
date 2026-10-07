package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/vibeci/vibeci/internal/agent"
	"github.com/vibeci/vibeci/internal/sandbox"
)

func js(s string) json.RawMessage { return json.RawMessage(s) }

// fileTools operate on the workspace through os.Root, so the model cannot
// read or write anything outside it (including via symlinks).
type fileTools struct {
	dir string
}

func (f *fileTools) open() (*os.Root, error) { return os.OpenRoot(f.dir) }

func (f *fileTools) read(ctx context.Context, in json.RawMessage) (agent.Result, error) {
	var a struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := agent.Decode(in, &a); err != nil {
		return agent.Errorf("%v", err), nil
	}
	p, err := cleanRel(a.Path)
	if err != nil {
		return agent.Errorf("%v", err), nil
	}
	root, err := f.open()
	if err != nil {
		return agent.Result{}, err
	}
	defer root.Close()
	st, err := root.Stat(p)
	if err != nil {
		return agent.Errorf("%v", trimRootErr(err)), nil
	}
	if st.IsDir() {
		entries, err := fs.ReadDir(root.FS(), p)
		if err != nil {
			return agent.Errorf("%v", trimRootErr(err)), nil
		}
		var sb strings.Builder
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			sb.WriteString(name + "\n")
		}
		return agent.OK(sb.String()), nil
	}
	if st.Size() > 16<<20 {
		return agent.Errorf("file is %d bytes; use the shell (head, sed -n, grep) to inspect parts of it", st.Size()), nil
	}
	data, err := root.ReadFile(p)
	if err != nil {
		return agent.Errorf("%v", trimRootErr(err)), nil
	}
	return agent.OK(numberedLines(data, a.StartLine, a.EndLine)), nil
}

// numberedLines renders lines start..end (1-based; at most 1000) of a file
// with line numbers, or a note for binary files.
func numberedLines(data []byte, start, end int) string {
	if strings.IndexByte(string(data[:min(len(data), 8000)]), 0) >= 0 {
		return fmt.Sprintf("binary file, %d bytes", len(data))
	}
	lines := strings.Split(string(data), "\n")
	start = max(1, start)
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if start > end {
		return fmt.Sprintf("[the file has %d lines]\n", len(lines))
	}
	if end-start+1 > 1000 {
		end = start + 999
	}
	var sb strings.Builder
	for i := start; i <= end; i++ {
		fmt.Fprintf(&sb, "%6d  %s\n", i, lines[i-1])
	}
	if end < len(lines) {
		fmt.Fprintf(&sb, "[... %d lines total; continue with start_line=%d ...]\n", len(lines), end+1)
	}
	return sb.String()
}

func (f *fileTools) write(ctx context.Context, in json.RawMessage) (agent.Result, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := agent.Decode(in, &a); err != nil {
		return agent.Errorf("%v", err), nil
	}
	p, err := cleanRel(a.Path)
	if err != nil {
		return agent.Errorf("%v", err), nil
	}
	if len(a.Content) > 4<<20 {
		return agent.Errorf("content too large"), nil
	}
	root, err := f.open()
	if err != nil {
		return agent.Result{}, err
	}
	defer root.Close()
	mode := os.FileMode(0o644)
	if st, err := root.Stat(p); err == nil {
		if st.IsDir() {
			return agent.Errorf("%s is a directory", p), nil
		}
		mode = st.Mode().Perm()
	}
	if dir := path.Dir(p); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return agent.Errorf("%v", trimRootErr(err)), nil
		}
	}
	if err := root.WriteFile(p, []byte(a.Content), mode); err != nil {
		return agent.Errorf("%v", trimRootErr(err)), nil
	}
	return agent.OK(fmt.Sprintf("wrote %s (%d bytes)", p, len(a.Content))), nil
}

func (f *fileTools) edit(ctx context.Context, in json.RawMessage) (agent.Result, error) {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := agent.Decode(in, &a); err != nil {
		return agent.Errorf("%v", err), nil
	}
	p, err := cleanRel(a.Path)
	if err != nil {
		return agent.Errorf("%v", err), nil
	}
	if a.OldString == "" || a.OldString == a.NewString {
		return agent.Errorf("old_string must be non-empty and differ from new_string"), nil
	}
	root, err := f.open()
	if err != nil {
		return agent.Result{}, err
	}
	defer root.Close()
	st, err := root.Stat(p)
	if err != nil {
		return agent.Errorf("%v", trimRootErr(err)), nil
	}
	data, err := root.ReadFile(p)
	if err != nil {
		return agent.Errorf("%v", trimRootErr(err)), nil
	}
	s := string(data)
	n := strings.Count(s, a.OldString)
	switch {
	case n == 0:
		return agent.Errorf("old_string not found in %s (it must match exactly, including whitespace)", p), nil
	case n > 1 && !a.ReplaceAll:
		return agent.Errorf("old_string occurs %d times in %s; include more surrounding context or set replace_all", n, p), nil
	}
	if a.ReplaceAll {
		s = strings.ReplaceAll(s, a.OldString, a.NewString)
	} else {
		s = strings.Replace(s, a.OldString, a.NewString, 1)
	}
	if err := root.WriteFile(p, []byte(s), st.Mode().Perm()); err != nil {
		return agent.Errorf("%v", trimRootErr(err)), nil
	}
	return agent.OK(fmt.Sprintf("edited %s (%d replacement(s))", p, map[bool]int{true: n, false: 1}[a.ReplaceAll])), nil
}

func trimRootErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %v", pe.Path, pe.Err)
	}
	return err
}

// shellTool runs a command in the agent's sandbox.
func shellTool(sb sandbox.Sandbox, maxTimeout time.Duration) *agent.Tool {
	return &agent.Tool{
		Name:        "shell",
		Description: "Run a shell command (sh -c) in /workspace inside the sandbox. Use it for git inspection, building, testing and searching. Output is truncated if long.",
		Schema:      js(`{"type":"object","properties":{"command":{"type":"string"},"timeout_seconds":{"type":"integer","description":"default 600"}},"required":["command"]}`),
		Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
			var a struct {
				Command        string `json:"command"`
				TimeoutSeconds int    `json:"timeout_seconds"`
			}
			if err := agent.Decode(in, &a); err != nil {
				return agent.Errorf("%v", err), nil
			}
			t := a.TimeoutSeconds
			if t <= 0 {
				t = 600
			}
			t = min(t, int(maxTimeout.Seconds()))
			res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: a.Command, TimeoutSec: t, MaxOutput: 1 << 20})
			if err != nil {
				if errors.Is(err, sandbox.ErrInvalid) {
					return agent.Errorf("%v", err), nil
				}
				return agent.Result{}, err
			}
			return agent.OK(res.Combined()), nil
		},
	}
}
