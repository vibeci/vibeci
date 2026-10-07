package jsonc

import (
	"strings"
	"testing"
)

func TestStandardize(t *testing.T) {
	src := `{
  // line comment with "quotes" and /* fake block
  "a": "http://x//y", /* block
  comment */ "b": [1, 2, 3,],
  "c": {"d": "e\"//not a comment",},
}`
	var v struct {
		A string `json:"a"`
		B []int  `json:"b"`
		C struct {
			D string `json:"d"`
		} `json:"c"`
	}
	if err := Unmarshal([]byte(src), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != "http://x//y" || len(v.B) != 3 || v.C.D != `e"//not a comment` {
		t.Fatalf("bad decode: %+v", v)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	err := Unmarshal([]byte("{\n  \"a\": 1,\n  \"typo\": 2\n}"), &v)
	if err == nil || !strings.Contains(err.Error(), "typo") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestErrorPosition(t *testing.T) {
	var v map[string]any
	err := Unmarshal([]byte("{\n  \"a\": 1\n  \"b\": 2\n}"), &v)
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("expected error on line 3, got %v", err)
	}
}

func TestUnterminated(t *testing.T) {
	if _, err := Standardize([]byte(`{"a": 1 /* oops`)); err == nil {
		t.Fatal("expected error")
	}
	var v any
	if err := Unmarshal([]byte(`{"a": 1} {"b": 2}`), &v); err == nil {
		t.Fatal("expected trailing data error")
	}
}
