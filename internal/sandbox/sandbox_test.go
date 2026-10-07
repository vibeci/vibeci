package sandbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateRequestValidation(t *testing.T) {
	good := CreateRequest{Profile: "default", Workspace: "jobs/j1/work", Objects: "mirrors/demo.git/objects", Cache: "jobs/j1/cache", Label: "j1"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []CreateRequest{
		{Profile: "default", Workspace: "jobs/../../etc"},
		{Profile: "default", Workspace: "/etc"},
		{Profile: "default", Workspace: "mirrors/demo.git"},
		{Profile: "default", Workspace: "jobs/j1/work", Objects: "mirrors/../state"},
		{Profile: "default", Workspace: "jobs/j1/work", Objects: "jobs/j1/work"},
		{Profile: "../x", Workspace: "jobs/j1/work"},
		{Profile: "default", Workspace: "jobs/j1/work", Cache: "jobs/j1/work"},
		{Profile: "default", Workspace: "jobs/j1/work", Label: "a b"},
	}
	for _, r := range bad {
		if err := r.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("expected rejection for %+v, got %v", r, err)
		}
	}
}

func TestExecRequestValidation(t *testing.T) {
	r := ExecRequest{Command: "ls", Workdir: "sub/../sub2", TimeoutSec: 99999}
	if err := r.Validate(time.Hour); !errors.Is(err, ErrInvalid) {
		t.Errorf("'..' in workdir must be rejected: %v", err)
	}
	r = ExecRequest{Command: "ls", Workdir: "/sub/dir/", TimeoutSec: 99999}
	if err := r.Validate(time.Hour); err != nil || r.Workdir != "sub/dir" || r.TimeoutSec != 3600 {
		t.Errorf("normalize: %+v %v", r, err)
	}
	r = ExecRequest{Command: "ls", Env: map[string]string{"BAD-KEY": "x"}}
	if err := r.Validate(time.Hour); err == nil {
		t.Error("bad env key accepted")
	}
}

func TestCapBuffer(t *testing.T) {
	c := newCapBuffer(100)
	for i := 0; i < 1000; i++ {
		c.Write([]byte("0123456789"))
	}
	s := c.String()
	if !c.Truncated() || !strings.HasPrefix(s, "0123456789") || !strings.HasSuffix(s, "0123456789") || !strings.Contains(s, "omitted") {
		t.Errorf("cap buffer: %q", s)
	}
	small := newCapBuffer(100)
	small.Write([]byte("hello"))
	if small.Truncated() || small.String() != "hello" {
		t.Errorf("small: %q", small.String())
	}
}

func TestDemux(t *testing.T) {
	var stream bytes.Buffer
	frame := func(kind byte, s string) {
		hdr := make([]byte, 8)
		hdr[0] = kind
		binary.BigEndian.PutUint32(hdr[4:], uint32(len(s)))
		stream.Write(hdr)
		stream.WriteString(s)
	}
	frame(1, "out1 ")
	frame(2, "err1")
	frame(1, "out2")
	var o, e bytes.Buffer
	if err := demux(&stream, &o, &e); err != nil {
		t.Fatal(err)
	}
	if o.String() != "out1 out2" || e.String() != "err1" {
		t.Errorf("demux: %q %q", o.String(), e.String())
	}
}

func TestLocalProvider(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "jobs/j1/work"), 0o755)
	p := &LocalProvider{DataRoot: root}
	s, err := p.Create(context.Background(), CreateRequest{Profile: "default", Workspace: "jobs/j1/work"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	res, err := s.Exec(context.Background(), ExecRequest{Command: "echo hi > f && cat f && echo oops >&2 && exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || strings.TrimSpace(res.Stdout) != "hi" || strings.TrimSpace(res.Stderr) != "oops" {
		t.Errorf("res: %+v", res)
	}
	res, err = s.Exec(context.Background(), ExecRequest{Command: "sleep 5", TimeoutSec: 1})
	if err != nil || !res.TimedOut {
		t.Errorf("timeout: %+v %v", res, err)
	}
}
