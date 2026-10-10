package handler

import (
	"testing"
	"time"
)

// TestFormatDuration Duration.String() 会输出 "1m3.482910293s" 这种
// 精度过高的形式，不适合直接展示给用户
func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{120 * time.Millisecond, "120ms"},
		{999 * time.Millisecond, "999ms"},
		{time.Second, "1.0s"},
		{1500 * time.Millisecond, "1.5s"},
		{59500 * time.Millisecond, "59.5s"},
		{time.Minute, "1m0s"},
		{90 * time.Second, "1m30s"},
		{3*time.Minute + 7*time.Second, "3m7s"},
	}
	for _, c := range cases {
		if got := formatDuration(c.d); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
