package llm

import (
	"bufio"
	"bytes"
	"io"
)

type sseEvent struct {
	Event string
	Data  []byte
}

// readSSE parses a server-sent-events stream and calls fn for each event.
// fn returning errStopStream ends parsing without error.
func readSSE(r io.Reader, fn func(sseEvent) error) error {
	br := bufio.NewReaderSize(r, 256<<10)
	var event string
	var data bytes.Buffer
	dispatch := func() error {
		if data.Len() == 0 && event == "" {
			return nil
		}
		ev := sseEvent{Event: event, Data: append([]byte(nil), data.Bytes()...)}
		event = ""
		data.Reset()
		return fn(ev)
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(line) == 0:
				if derr := dispatch(); derr != nil {
					if derr == errStopStream {
						return nil
					}
					return derr
				}
			case line[0] == ':':
				// comment / keepalive
			default:
				field, value, _ := bytes.Cut(line, []byte(":"))
				value = bytes.TrimPrefix(value, []byte(" "))
				switch string(field) {
				case "event":
					event = string(value)
				case "data":
					if data.Len() > 0 {
						data.WriteByte('\n')
					}
					data.Write(value)
				}
			}
		}
		if err == io.EOF {
			if derr := dispatch(); derr != nil && derr != errStopStream {
				return derr
			}
			return nil
		}
		if err != nil {
			return transient(err)
		}
	}
}

type stopStream struct{}

func (stopStream) Error() string { return "stop stream" }

var errStopStream error = stopStream{}
