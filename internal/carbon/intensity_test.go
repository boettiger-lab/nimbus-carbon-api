package carbon

import "testing"

func TestBerkeleyIntensity(t *testing.T) {
	if BerkeleyIntensity != 0.198 {
		t.Errorf("BerkeleyIntensity = %v, want 0.198", BerkeleyIntensity)
	}
}

func TestGramsPerHour(t *testing.T) {
	got := GramsPerHour(100, BerkeleyIntensity)
	want := 19.8
	if got != want {
		t.Errorf("GramsPerHour(100, %v) = %v, want %v", BerkeleyIntensity, got, want)
	}
}

func TestMgPerToken(t *testing.T) {
	got := MgPerToken(100, BerkeleyIntensity, 50)
	want := 100 * BerkeleyIntensity * 0.2778 / 50
	if got != want {
		t.Errorf("MgPerToken(100, %v, 50) = %v, want %v", BerkeleyIntensity, got, want)
	}

	if z := MgPerToken(100, BerkeleyIntensity, 0); z != 0 {
		t.Errorf("MgPerToken with 0 tokens/sec = %v, want 0", z)
	}
}
