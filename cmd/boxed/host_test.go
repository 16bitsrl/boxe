package main

import "testing"

// By default the awake VMs may have the host's RAM less what the host
// itself needs: 10%, at least 1G.
func TestHostMemLimit(t *testing.T) {
	const gib = int64(1) << 30
	for _, c := range []struct {
		flag  string
		total int64
		want  int
	}{
		{"", 64 * gib, 64*1024 - 64*1024/10},
		{"", 8 * gib, 7 * 1024},     // 10% would be less than 1G
		{"0", 8 * gib, 0},           // no limit
		{"16G", 8 * gib, 16 * 1024}, // more, with swap on the host
		{"", 0, 0},                  // RAM unknown: no limit
	} {
		got, err := hostMemLimit(c.flag, c.total)
		if err != nil || got != c.want {
			t.Errorf("hostMemLimit(%q, %d) = %d, %v; want %d", c.flag, c.total, got, err, c.want)
		}
	}
	if _, err := hostMemLimit("lots", 8*gib); err == nil {
		t.Error("no error for a bad size")
	}
}
