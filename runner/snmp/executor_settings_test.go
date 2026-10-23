package snmp

import (
	"testing"
	"time"
)

func TestExecutorSettings(t *testing.T) {
	static := Config{Timeout: 2 * time.Second, Retries: 1}
	live := func(d time.Duration, r int) func() (time.Duration, int) {
		return func() (time.Duration, int) { return d, r }
	}
	zero := 0
	for _, tc := range []struct {
		name        string
		live        func() (time.Duration, int)
		override    *int
		wantTimeout time.Duration
		wantRetries int
	}{
		{"nil Live uses static config", nil, nil, 2 * time.Second, 1},
		{"Live values", live(500*time.Millisecond, 3), nil, 500 * time.Millisecond, 3},
		{"Live retries 0 is valid", live(time.Second, 0), nil, time.Second, 0},
		{"Live timeout 0 falls back", live(0, 3), nil, 2 * time.Second, 3},
		{"Live timeout negative falls back", live(-time.Second, 3), nil, 2 * time.Second, 3},
		{"Live retries negative falls back", live(time.Second, -1), nil, time.Second, 1},
		{"Target.Retries beats Live", live(time.Second, 3), &zero, time.Second, 0},
		{"Target.Retries beats static", nil, &zero, 2 * time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := static
			cfg.Live = tc.live
			e := &goSNMPExecutor{cfg: cfg}
			timeout, retries := e.settings(tc.override)
			if timeout != tc.wantTimeout || retries != tc.wantRetries {
				t.Errorf("settings = (%s, %d), want (%s, %d)", timeout, retries, tc.wantTimeout, tc.wantRetries)
			}
		})
	}
}

func TestExecutorSettings_LiveReadPerCall(t *testing.T) {
	cur := time.Second
	e := &goSNMPExecutor{cfg: Config{Timeout: 2 * time.Second, Retries: 1, Live: func() (time.Duration, int) { return cur, 1 }}}
	if timeout, _ := e.settings(nil); timeout != time.Second {
		t.Fatalf("timeout = %s, want 1s", timeout)
	}
	cur = 3 * time.Second
	if timeout, _ := e.settings(nil); timeout != 3*time.Second {
		t.Errorf("timeout after reload = %s, want 3s", timeout)
	}
}
