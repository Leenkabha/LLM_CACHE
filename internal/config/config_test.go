package config

import "testing"

func TestLoadTopK(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    int
		invalid bool
	}{
		{"", 1, false}, {"1", 1, false}, {"3", 3, false}, {"0", 0, true}, {"-2", 0, true}, {"abc", 0, true}, {"1.5", 0, true}, {"999999999999999999999999999999", 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("CACHE_TOP_K", tc.value)
			cfg, err := Load()
			if (err != nil) != tc.invalid {
				t.Fatalf("Load() error=%v", err)
			}
			if err == nil && cfg.TopK != tc.want {
				t.Fatalf("TopK=%d, want %d", cfg.TopK, tc.want)
			}
		})
	}
}
