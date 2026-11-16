package progression

import "testing"

func TestLevelFor(t *testing.T) {
	thresholds := []int64{100, 300}
	for xp, want := range map[int64]int{0: 1, 99: 1, 100: 2, 299: 2, 300: 3, 5000: 3} {
		if got := LevelFor(thresholds, xp); got != want {
			t.Fatalf("LevelFor(%d)=%d want %d", xp, got, want)
		}
	}
}
