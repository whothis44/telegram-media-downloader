package app

import "testing"

// ix.12.3: the report's per-server figures, by server number.
func TestIxDCList(t *testing.T) {
	got := ixDCList(map[int]int64{4: 10 << 20, 1: 820 << 20}, map[int]int{1: 3, 5: 2})
	if want := "DC1 820 MB (3 limit waits), DC4 10 MB, DC5 0 MB (2 limit waits)"; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	if got := ixDCList(nil, nil); got != "" {
		t.Fatalf("%q", got)
	}
}
