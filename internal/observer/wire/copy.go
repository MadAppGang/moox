package wire

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// The helpers in this file exist for one reason: the codec must be LOSSLESS,
// and the two ways to lose information silently in Go JSON are aliasing a
// pointer and conflating a nil slice with an empty one.
//
// Every copy* helper below therefore preserves nil-ness exactly. A nil slice
// encodes as null and decodes back to nil; an empty slice encodes as [] and
// decodes back to empty. That is not pedantry — the whole nullable design of
// this schema rests on "absent" and "empty" being different answers, and a
// helper that quietly returned []string{} for nil would break the round-trip
// identity in a way no single test would obviously localise.
//
// The corollary for callers: the CORE decides whether a list field is nil or
// empty, and it should initialise formerNames, listedBy, sources and advisories
// to empty slices so the JSON reads "[]" rather than "null". The codec does not
// make that choice on the core's behalf.

func copyStringPtr(v *string) *string {
	if v == nil {
		return nil
	}
	s := *v
	return &s
}

func copyIntPtr(v *int) *int {
	if v == nil {
		return nil
	}
	i := *v
	return &i
}

func copyInt64Ptr(v *int64) *int64 {
	if v == nil {
		return nil
	}
	i := *v
	return &i
}

func copyBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	b := *v
	return &b
}

func copyStrings(v []string) []string {
	if v == nil {
		return nil
	}
	out := make([]string, len(v))
	copy(out, v)
	return out
}

// formatTimePtr renders a nullable timestamp. A nil time stays nil — it does
// NOT become the zero time, which would serialise as year 1 and compute an age
// of two millennia.
func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := observer.FormatTime(*t)
	return &s
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// jsonFieldNames returns the json tag names a struct type declares, so the
// unknown-field detector is derived from the structs themselves rather than
// from a hand-maintained duplicate list that would rot.
func jsonFieldNames(v any) map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := tag
		for j := 0; j < len(tag); j++ {
			if tag[j] == ',' {
				name = tag[:j]
				break
			}
		}
		if name != "" {
			out[name] = true
		}
	}
	return out
}

func snapshotKnownFields() map[string]map[string]bool {
	return map[string]map[string]bool{
		"root":    jsonFieldNames(Snapshot{}),
		"session": jsonFieldNames(Session{}),
	}
}

// frameKnownFields carries THREE levels, not two, because a snapshot frame
// nests the whole S1 envelope under "snapshot": without the third level a
// vendor-driven addition inside that envelope would travel over the socket
// unreported, while the identical bytes read from `tt sessions --json` would be
// named. P8 decodes frames through DecodeFrame, so the gap would have been
// invisible exactly where §7.5 puts the load.
func frameKnownFields() map[string]map[string]bool {
	return map[string]map[string]bool{
		"root":     jsonFieldNames(Frame{}),
		"session":  jsonFieldNames(Session{}),
		"snapshot": jsonFieldNames(Snapshot{}),
	}
}

// compile-time use of encoding/json so the import is not accidental: the
// unknown-field detector relies on json.RawMessage, which reflect alone cannot
// give us.
var _ = json.RawMessage(nil)
