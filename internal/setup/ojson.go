package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ojson is a JSON value that keeps object key order, so merging two hooks
// into a user's settings.json does not reorder everything else in it.
type ojson struct {
	kind byte // '{', '[', or 0 for a scalar
	obj  []okv
	arr  []*ojson
	raw  json.RawMessage // scalar
}

type okv struct {
	key string
	val *ojson
}

func parseOJSON(b []byte) (*ojson, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := readOJSON(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("setup: trailing data after the JSON value")
	}
	return v, nil
}

func readOJSON(dec *json.Decoder) (*ojson, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			v := &ojson{kind: '{'}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("setup: object key %v", kt)
				}
				val, err := readOJSON(dec)
				if err != nil {
					return nil, err
				}
				v.obj = append(v.obj, okv{k, val})
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return v, nil
		case '[':
			v := &ojson{kind: '['}
			for dec.More() {
				el, err := readOJSON(dec)
				if err != nil {
					return nil, err
				}
				v.arr = append(v.arr, el)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return v, nil
		}
		return nil, fmt.Errorf("setup: unexpected %v", t)
	default:
		raw, err := marshalPlain(t)
		if err != nil {
			return nil, err
		}
		return &ojson{raw: raw}, nil
	}
}

func ostr(s string) *ojson {
	raw, _ := marshalPlain(s)
	return &ojson{raw: raw}
}

// marshalPlain is json.Marshal without HTML escaping (a command's `&&` stays
// `&&`, not \u0026\u0026).
func marshalPlain(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func (v *ojson) get(key string) *ojson {
	if v == nil || v.kind != '{' {
		return nil
	}
	for _, kv := range v.obj {
		if kv.key == key {
			return kv.val
		}
	}
	return nil
}

func (v *ojson) set(key string, val *ojson) {
	for i, kv := range v.obj {
		if kv.key == key {
			v.obj[i].val = val
			return
		}
	}
	v.obj = append(v.obj, okv{key, val})
}

func (v *ojson) del(key string) {
	for i, kv := range v.obj {
		if kv.key == key {
			v.obj = append(v.obj[:i], v.obj[i+1:]...)
			return
		}
	}
}

// str is a scalar string's value ("" otherwise).
func (v *ojson) str() string {
	if v == nil || v.kind != 0 {
		return ""
	}
	var s string
	if json.Unmarshal(v.raw, &s) != nil {
		return ""
	}
	return s
}

// encode writes v indented by two spaces, with a trailing newline.
func (v *ojson) encode() []byte {
	var b strings.Builder
	v.write(&b, 0)
	b.WriteByte('\n')
	return []byte(b.String())
}

func (v *ojson) write(b *strings.Builder, depth int) {
	ind := func(d int) { b.WriteString(strings.Repeat("  ", d)) }
	switch v.kind {
	case '{':
		if len(v.obj) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, kv := range v.obj {
			ind(depth + 1)
			k, _ := marshalPlain(kv.key)
			b.Write(k)
			b.WriteString(": ")
			kv.val.write(b, depth+1)
			if i < len(v.obj)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		ind(depth)
		b.WriteByte('}')
	case '[':
		if len(v.arr) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, el := range v.arr {
			ind(depth + 1)
			el.write(b, depth+1)
			if i < len(v.arr)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		ind(depth)
		b.WriteByte(']')
	default:
		b.Write(v.raw)
	}
}
