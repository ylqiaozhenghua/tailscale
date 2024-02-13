// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package views

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
)

type viewStruct struct {
	Int        int
	Addrs      Slice[netip.Prefix]
	Strings    Slice[string]
	AddrsPtr   *Slice[netip.Prefix] `json:",omitempty"`
	StringsPtr *Slice[string]       `json:",omitempty"`
}

func BenchmarkSliceIteration(b *testing.B) {
	var data []viewStruct
	for i := 0; i < 10000; i++ {
		data = append(data, viewStruct{Int: i})
	}
	b.ResetTimer()
	b.Run("Len", func(b *testing.B) {
		b.ReportAllocs()
		dv := SliceOf(data)
		for it := 0; it < b.N; it++ {
			sum := 0
			for i := 0; i < dv.Len(); i++ {
				sum += dv.At(i).Int
			}
		}
	})
	b.Run("Cached-Len", func(b *testing.B) {
		b.ReportAllocs()
		dv := SliceOf(data)
		for it := 0; it < b.N; it++ {
			sum := 0
			for i, n := 0, dv.Len(); i < n; i++ {
				sum += dv.At(i).Int
			}
		}
	})
	b.Run("direct", func(b *testing.B) {
		b.ReportAllocs()
		for it := 0; it < b.N; it++ {
			sum := 0
			for _, d := range data {
				sum += d.Int
			}
		}
	})
}

func TestViewsJSON(t *testing.T) {
	mustCIDR := func(cidrs ...string) (out []netip.Prefix) {
		for _, cidr := range cidrs {
			out = append(out, netip.MustParsePrefix(cidr))
		}
		return
	}
	ipp := SliceOf(mustCIDR("192.168.0.0/24"))
	ss := SliceOf([]string{"bar"})
	tests := []struct {
		name     string
		in       viewStruct
		wantJSON string
	}{
		{
			name:     "empty",
			in:       viewStruct{},
			wantJSON: `{"Int":0,"Addrs":null,"Strings":null}`,
		},
		{
			name: "everything",
			in: viewStruct{
				Int:        1234,
				Addrs:      ipp,
				AddrsPtr:   &ipp,
				StringsPtr: &ss,
				Strings:    ss,
			},
			wantJSON: `{"Int":1234,"Addrs":["192.168.0.0/24"],"Strings":["bar"],"AddrsPtr":["192.168.0.0/24"],"StringsPtr":["bar"]}`,
		},
	}

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "")
	for _, tc := range tests {
		buf.Reset()
		if err := encoder.Encode(&tc.in); err != nil {
			t.Fatal(err)
		}
		b := buf.Bytes()
		gotJSON := strings.TrimSpace(string(b))
		if tc.wantJSON != gotJSON {
			t.Fatalf("JSON: %v; want: %v", gotJSON, tc.wantJSON)
		}
		var got viewStruct
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.in) {
			t.Fatalf("unmarshal resulted in different output: %+v; want %+v", got, tc.in)
		}
	}
}

func TestViewUtils(t *testing.T) {
	v := SliceOf([]string{"foo", "bar"})
	c := qt.New(t)

	c.Check(v.ContainsFunc(func(s string) bool { return strings.HasPrefix(s, "f") }), qt.Equals, true)
	c.Check(v.ContainsFunc(func(s string) bool { return strings.HasPrefix(s, "g") }), qt.Equals, false)
	c.Check(v.IndexFunc(func(s string) bool { return strings.HasPrefix(s, "b") }), qt.Equals, 1)
	c.Check(v.IndexFunc(func(s string) bool { return strings.HasPrefix(s, "z") }), qt.Equals, -1)
	c.Check(SliceContains(v, "bar"), qt.Equals, true)
	c.Check(SliceContains(v, "baz"), qt.Equals, false)
	c.Check(SliceContainsFunc(v, func(s string) bool { return strings.HasPrefix(s, "f") }), qt.Equals, true)
	c.Check(SliceContainsFunc(v, func(s string) bool { return len(s) > 3 }), qt.Equals, false)
	c.Check(SliceEqualAnyOrder(v, v), qt.Equals, true)
	c.Check(SliceEqualAnyOrder(v, SliceOf([]string{"bar", "foo"})), qt.Equals, true)
	c.Check(SliceEqualAnyOrder(v, SliceOf([]string{"foo"})), qt.Equals, false)
	c.Check(SliceEqualAnyOrder(SliceOf([]string{"a", "a", "b"}), SliceOf([]string{"a", "b", "b"})), qt.Equals, false)

	c.Check(SliceEqualAnyOrder(
		SliceOf([]string{"a", "b", "c"}).SliceFrom(1),
		SliceOf([]string{"b", "c"})),
		qt.Equals, true)
	c.Check(SliceEqualAnyOrder(
		SliceOf([]string{"a", "b", "c"}).Slice(1, 2),
		SliceOf([]string{"b", "c"}).SliceTo(1)),
		qt.Equals, true)
}

func TestLenIter(t *testing.T) {
	orig := []string{"foo", "bar"}
	var got []string
	v := SliceOf(orig)
	for i := range v.LenIter() {
		got = append(got, v.At(i))
	}
	if !reflect.DeepEqual(orig, got) {
		t.Errorf("got %q; want %q", got, orig)
	}
	x := 0
	n := testing.AllocsPerRun(10000, func() {
		for range v.LenIter() {
			x++
		}
	})
	if n > 0 {
		t.Errorf("allocs = %v; want 0", n)
	}
}

func TestSliceEqual(t *testing.T) {
	a := SliceOf([]string{"foo", "bar"})
	b := SliceOf([]string{"foo", "bar"})
	if !SliceEqual(a, b) {
		t.Errorf("got a != b")
	}
	if !SliceEqual(a.SliceTo(0), b.SliceTo(0)) {
		t.Errorf("got a[:0] != b[:0]")
	}
	if SliceEqual(a.SliceTo(2), a.SliceTo(1)) {
		t.Error("got a[:2] == a[:1]")
	}
}

// TestSliceMapKey tests that the MapKey method returns the same key for slices
// with the same underlying slice and different keys for different slices or
// with same underlying slice but different bounds.
func TestSliceMapKey(t *testing.T) {
	underlying := []string{"foo", "bar"}
	nilSlice := SliceOf[string](nil)
	empty := SliceOf([]string{})
	u1 := SliceOf(underlying)
	u2 := SliceOf(underlying)
	u3 := SliceOf([]string{"foo", "bar"}) // different underlying slice

	sub1 := u1.Slice(0, 1)
	sub2 := u1.Slice(1, 2)
	sub3 := u1.Slice(0, 2)

	wantSame := []Slice[string]{u1, u2, sub3}
	for i := 1; i < len(wantSame); i++ {
		s0, si := wantSame[0], wantSame[i]
		k0 := s0.MapKey()
		ki := si.MapKey()
		if ki != k0 {
			t.Fatalf("wantSame[%d, %+v, %q) != wantSame[0, %+v, %q)", i, ki, si.AsSlice(), k0, s0.AsSlice())
		}
	}

	wantDiff := []Slice[string]{nilSlice, empty, sub1, sub2, sub3, u3}
	for i := 0; i < len(wantDiff); i++ {
		for j := i + 1; j < len(wantDiff); j++ {
			si, sj := wantDiff[i], wantDiff[j]
			ki, kj := wantDiff[i].MapKey(), wantDiff[j].MapKey()
			if ki == kj {
				t.Fatalf("wantDiff[%d, %+v, %q] == wantDiff[%d, %+v, %q] ", i, ki, si.AsSlice(), j, kj, sj.AsSlice())
			}
		}
	}
}
