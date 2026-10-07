package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
)

// Merge3 merges two descendants of a JSON document, ours and theirs, of
// their common ancestor base: the changes each side made since base are
// combined object member by object member. Where both sides changed the
// same value differently, theirs wins. A nil document is absent; the result
// is nil if the merged document is absent.
func Merge3(base, ours, theirs []byte) ([]byte, error) {
	var docs [3]any
	for i, b := range [][]byte{base, ours, theirs} {
		var err error
		if docs[i], err = decode(b); err != nil {
			return nil, err
		}
	}
	m := merge3(docs[0], docs[1], docs[2])
	if isAbsent(m) {
		return nil, nil
	}
	return json.Marshal(m)
}

// EqualJSON reports whether a and b hold the same JSON value (nil is
// absent; invalid JSON is equal to nothing).
func EqualJSON(a, b []byte) bool {
	va, err1 := decode(a)
	vb, err2 := decode(b)
	return err1 == nil && err2 == nil && reflect.DeepEqual(va, vb)
}

func decode(b []byte) (any, error) {
	if b == nil {
		return absent{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

// absent marks a missing value (as opposed to JSON null).
type absent struct{}

func isAbsent(v any) bool {
	_, ok := v.(absent)
	return ok
}

func merge3(base, ours, theirs any) any {
	switch {
	case reflect.DeepEqual(ours, theirs), reflect.DeepEqual(theirs, base):
		return ours
	case reflect.DeepEqual(ours, base):
		return theirs
	}
	om, ok1 := ours.(map[string]any)
	tm, ok2 := theirs.(map[string]any)
	if !ok1 || !ok2 {
		return theirs
	}
	bm, _ := base.(map[string]any) // an absent or non-object base merges as {}
	keys := map[string]bool{}
	for _, m := range []map[string]any{bm, om, tm} {
		for k := range m {
			keys[k] = true
		}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	out := make(map[string]any, len(keys))
	for _, k := range sorted {
		if v := merge3(member(bm, k), member(om, k), member(tm, k)); !isAbsent(v) {
			out[k] = v
		}
	}
	return out
}

func member(m map[string]any, k string) any {
	if v, ok := m[k]; ok {
		return v
	}
	return absent{}
}
