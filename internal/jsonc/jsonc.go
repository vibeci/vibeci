// Package jsonc decodes JSON with comments and trailing commas (the dialect
// used by VS Code and opencode config files) on top of encoding/json.
package jsonc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Standardize converts JSONC to plain JSON. Comments and trailing commas are
// overwritten with spaces, so byte offsets (and therefore line/column numbers
// in error messages) are preserved.
func Standardize(src []byte) ([]byte, error) {
	out := make([]byte, len(src))
	copy(out, src)

	const (
		normal = iota
		inString
		lineComment
		blockComment
	)
	state := normal
	lastComma := -1 // position of a comma that might turn out to be trailing

	for i := 0; i < len(out); i++ {
		c := out[i]
		switch state {
		case normal:
			switch c {
			case '"':
				state = inString
				lastComma = -1
			case '/':
				if i+1 < len(out) && out[i+1] == '/' {
					out[i], out[i+1] = ' ', ' '
					i++
					state = lineComment
				} else if i+1 < len(out) && out[i+1] == '*' {
					out[i], out[i+1] = ' ', ' '
					i++
					state = blockComment
				} else {
					lastComma = -1
				}
			case ',':
				lastComma = i
			case '}', ']':
				if lastComma >= 0 {
					out[lastComma] = ' '
				}
				lastComma = -1
			case ' ', '\t', '\r', '\n':
				// whitespace does not cancel a pending trailing comma
			default:
				lastComma = -1
			}
		case inString:
			switch c {
			case '\\':
				i++ // skip the escaped byte
			case '"':
				state = normal
			}
		case lineComment:
			if c == '\n' {
				state = normal
			} else {
				out[i] = ' '
			}
		case blockComment:
			if c == '*' && i+1 < len(out) && out[i+1] == '/' {
				out[i], out[i+1] = ' ', ' '
				i++
				state = normal
			} else if c != '\n' {
				out[i] = ' '
			}
		}
	}
	if state == blockComment {
		return nil, errors.New("jsonc: unterminated block comment")
	}
	if state == inString {
		return nil, errors.New("jsonc: unterminated string")
	}
	return out, nil
}

// Unmarshal decodes JSONC into v. Unknown object fields are rejected so that
// typos in configuration files are reported instead of silently ignored.
func Unmarshal(src []byte, v any) error {
	std, err := Standardize(src)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(std))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return annotate(std, dec.InputOffset(), err)
	}
	// Only whitespace may follow the top-level value.
	rest := bytes.TrimSpace(std[dec.InputOffset():])
	if len(rest) > 0 {
		return annotate(std, dec.InputOffset(), errors.New("unexpected data after top-level value"))
	}
	return nil
}

func annotate(src []byte, fallback int64, err error) error {
	off := fallback
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		off = se.Offset
	case errors.As(err, &te):
		off = te.Offset
	}
	if off < 0 || off > int64(len(src)) {
		return err
	}
	line, col := 1, 1
	for _, b := range src[:off] {
		if b == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return fmt.Errorf("line %d, column %d: %w", line, col, err)
}
