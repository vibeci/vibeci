package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// dockerAPI is a minimal Docker Engine API client (unix socket or plain
// tcp), standard library only.
type dockerAPI struct {
	client *http.Client
	// stream is used for hijacked exec streams (no connection reuse).
	stream *http.Client
	base   string
}

const dockerAPIVersion = "v1.45" // volume subpaths need >= 1.45

func newDockerAPI(host string) (*dockerAPI, error) {
	var dial func(ctx context.Context, network, addr string) (net.Conn, error)
	base := "http://docker/" + dockerAPIVersion
	switch {
	case strings.HasPrefix(host, "unix://"):
		sock := strings.TrimPrefix(host, "unix://")
		dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}
	case strings.HasPrefix(host, "tcp://"):
		addr := strings.TrimPrefix(host, "tcp://")
		base = "http://" + addr + "/" + dockerAPIVersion
		dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}
	default:
		return nil, fmt.Errorf("unsupported docker host %q", host)
	}
	mk := func(keepAlive bool) *http.Client {
		return &http.Client{Transport: &http.Transport{
			DialContext:        dial,
			DisableKeepAlives:  !keepAlive,
			MaxIdleConns:       8,
			IdleConnTimeout:    30 * time.Second,
			DisableCompression: true,
		}}
	}
	return &dockerAPI{client: mk(true), stream: mk(false), base: base}, nil
}

type dockerError struct {
	Status  int
	Message string
}

func (e *dockerError) Error() string { return fmt.Sprintf("docker: HTTP %d: %s", e.Status, e.Message) }

func isNotFound(err error) bool {
	var de *dockerError
	return errors.As(err, &de) && de.Status == http.StatusNotFound
}

func (d *dockerAPI) do(ctx context.Context, method, p string, q url.Values, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := d.base + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var m struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &m) != nil || m.Message == "" {
			m.Message = strings.TrimSpace(string(b))
		}
		return &dockerError{Status: resp.StatusCode, Message: m.Message}
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

func (d *dockerAPI) version(ctx context.Context) (string, error) {
	var v struct {
		Version    string `json:"Version"`
		APIVersion string `json:"ApiVersion"`
	}
	if err := d.do(ctx, http.MethodGet, "/version", nil, nil, &v); err != nil {
		return "", err
	}
	return v.Version + " (api " + v.APIVersion + ")", nil
}

func (d *dockerAPI) imageExists(ctx context.Context, ref string) (bool, error) {
	err := d.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, nil)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

func (d *dockerAPI) pull(ctx context.Context, ref string) error {
	name, tag := ref, "latest"
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		name, tag = ref[:at], ref[at+1:]
	} else if c := strings.LastIndex(ref, ":"); c > strings.LastIndex(ref, "/") {
		name, tag = ref[:c], ref[c+1:]
	}
	q := url.Values{"fromImage": {name}, "tag": {tag}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base+"/images/create?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("docker pull: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &dockerError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var m struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.Error != "" {
			return fmt.Errorf("docker pull %s: %s", ref, m.Error)
		}
	}
	return sc.Err()
}

func (d *dockerAPI) networkExists(ctx context.Context, name string) (bool, error) {
	err := d.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, nil, nil)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

type containerSpec struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd"`
	Entrypoint []string          `json:"Entrypoint"`
	User       string            `json:"User"`
	WorkingDir string            `json:"WorkingDir"`
	Env        []string          `json:"Env"`
	Labels     map[string]string `json:"Labels"`
	StopSignal string            `json:"StopSignal,omitempty"`
	HostConfig hostConfig        `json:"HostConfig"`
}

type hostConfig struct {
	NetworkMode    string            `json:"NetworkMode"`
	CapDrop        []string          `json:"CapDrop"`
	SecurityOpt    []string          `json:"SecurityOpt"`
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	Privileged     bool              `json:"Privileged"`
	Init           bool              `json:"Init"`
	IpcMode        string            `json:"IpcMode"`
	CgroupnsMode   string            `json:"CgroupnsMode,omitempty"`
	Memory         int64             `json:"Memory,omitempty"`
	MemorySwap     int64             `json:"MemorySwap,omitempty"`
	NanoCpus       int64             `json:"NanoCpus,omitempty"`
	PidsLimit      int64             `json:"PidsLimit,omitempty"`
	OomScoreAdj    int               `json:"OomScoreAdj,omitempty"`
	Tmpfs          map[string]string `json:"Tmpfs"`
	Mounts         []mount           `json:"Mounts"`
	LogConfig      logConfig         `json:"LogConfig"`
	AutoRemove     bool              `json:"AutoRemove"`
}

type logConfig struct {
	Type string `json:"Type"`
}

type mount struct {
	Type          string         `json:"Type"`
	Source        string         `json:"Source"`
	Target        string         `json:"Target"`
	ReadOnly      bool           `json:"ReadOnly"`
	VolumeOptions *volumeOptions `json:"VolumeOptions,omitempty"`
}

type volumeOptions struct {
	NoCopy  bool   `json:"NoCopy"`
	Subpath string `json:"Subpath,omitempty"`
}

func (d *dockerAPI) createContainer(ctx context.Context, name string, spec *containerSpec) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	if err := d.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, spec, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (d *dockerAPI) startContainer(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
}

func (d *dockerAPI) restartContainer(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodPost, "/containers/"+id+"/restart", url.Values{"t": {"0"}}, nil, nil)
}

func (d *dockerAPI) removeContainer(ctx context.Context, id string) error {
	err := d.do(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}, "v": {"1"}}, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

type containerSummary struct {
	ID      string            `json:"Id"`
	Labels  map[string]string `json:"Labels"`
	Created int64             `json:"Created"`
}

// listManaged lists the sandbox containers of one deployment (see
// DockerProvider.Instance).
func (d *dockerAPI) listManaged(ctx context.Context, instance string) ([]containerSummary, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {"vibeci.managed=true", "vibeci.instance=" + instance}})
	var out []containerSummary
	err := d.do(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(filters)}}, nil, &out)
	return out, err
}

// exec runs cmd in a running container, demultiplexing stdout/stderr.
func (d *dockerAPI) exec(ctx context.Context, id string, cmd, env []string, workdir string, maxOut int) (*ExecResult, error) {
	var created struct {
		ID string `json:"Id"`
	}
	body := map[string]any{
		"AttachStdin": false, "AttachStdout": true, "AttachStderr": true, "Tty": false,
		"Cmd": cmd, "Env": env, "WorkingDir": workdir,
	}
	if err := d.do(ctx, http.MethodPost, "/containers/"+id+"/exec", nil, body, &created); err != nil {
		return nil, err
	}
	start := time.Now()
	b, _ := json.Marshal(map[string]any{"Detach": false, "Tty": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base+"/exec/"+created.ID+"/start", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker exec start: %w", err)
	}
	stdout, stderr := newCapBuffer(maxOut), newCapBuffer(maxOut)
	err = demux(resp.Body, stdout, stderr)
	resp.Body.Close()
	if err != nil && ctx.Err() == nil {
		return nil, fmt.Errorf("docker exec stream: %w", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	res := &ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), Truncated: stdout.Truncated() || stderr.Truncated()}
	for i := 0; i < 50; i++ {
		var info struct {
			Running  bool `json:"Running"`
			ExitCode int  `json:"ExitCode"`
		}
		if err := d.do(ctx, http.MethodGet, "/exec/"+created.ID+"/json", nil, nil, &info); err != nil {
			return nil, err
		}
		if !info.Running {
			res.ExitCode = info.ExitCode
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

// demux splits Docker's multiplexed stream (8-byte frame headers).
func demux(r io.Reader, stdout, stderr io.Writer) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		size := int64(binary.BigEndian.Uint32(hdr[4:8]))
		var w io.Writer
		switch hdr[0] {
		case 1:
			w = stdout
		case 2:
			w = stderr
		default:
			w = io.Discard
		}
		if _, err := io.CopyN(w, br, size); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
