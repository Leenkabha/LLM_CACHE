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

// A malformed SIMILARITY_THRESHOLD must fail startup rather than silently
// becoming 0.0, which would make nearly every query miss with no warning.
func TestLoadSimilarityThreshold(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    float64
		invalid bool
	}{
		{"", 0.25, false}, {"0", 0, false}, {"0.25", 0.25, false}, {"1", 1, false},
		{"-1", -1, false}, {"abc", 0, true}, {"0.25.5", 0, true}, {"", 0.25, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("SIMILARITY_THRESHOLD", tc.value)
			cfg, err := Load()
			if (err != nil) != tc.invalid {
				t.Fatalf("Load() error=%v, want invalid=%v", err, tc.invalid)
			}
			if err == nil && cfg.Threshold != tc.want {
				t.Fatalf("Threshold=%v, want %v", cfg.Threshold, tc.want)
			}
		})
	}
}

// A malformed CACHE_CAPACITY must fail startup. Previously it silently became
// 0, which enforceCapacity treats as "unlimited" -- silently disabling
// eviction instead of reporting the typo.
func TestLoadCacheCapacity(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    int
		invalid bool
	}{
		{"", 1000, false}, {"0", 0, false}, {"-1", -1, false}, {"50", 50, false}, {"abc", 0, true}, {"1.5", 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("CACHE_CAPACITY", tc.value)
			cfg, err := Load()
			if (err != nil) != tc.invalid {
				t.Fatalf("Load() error=%v, want invalid=%v", err, tc.invalid)
			}
			if err == nil && cfg.Capacity != tc.want {
				t.Fatalf("Capacity=%d, want %d", cfg.Capacity, tc.want)
			}
		})
	}
}
