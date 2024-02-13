// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package metrics

import (
	"os"
	"runtime"
	"testing"

	"tailscale.com/tstest"
)

func TestLabelMap(t *testing.T) {
	var m LabelMap
	m.GetIncrFunc("foo")(1)
	m.GetIncrFunc("bar")(2)
	if g, w := m.Get("foo").Value(), int64(1); g != w {
		t.Errorf("foo = %v; want %v", g, w)
	}
	if g, w := m.Get("bar").Value(), int64(2); g != w {
		t.Errorf("bar = %v; want %v", g, w)
	}
}

func TestCurrentFileDescriptors(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("skipping on %v", runtime.GOOS)
	}
	n := CurrentFDs()
	if n < 3 {
		t.Fatalf("got %v; want >= 3", n)
	}

	err := tstest.MinAllocsPerRun(t, 0, func() {
		n = CurrentFDs()
	})
	if err != nil {
		t.Fatal(err)
	}

	// Open some FDs.
	const extra = 10
	for i := 0; i < extra; i++ {
		f, err := os.Open("/proc/self/stat")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		t.Logf("fds for #%v = %v", i, CurrentFDs())
	}

	n2 := CurrentFDs()
	if n2 < n+extra {
		t.Errorf("fds changed from %v => %v, want to %v", n, n2, n+extra)
	}
}

func BenchmarkCurrentFileDescriptors(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = CurrentFDs()
	}
}
