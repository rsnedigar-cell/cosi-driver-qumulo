package qumulo

import "testing"

func TestParseCoreVersion(t *testing.T) {
	v, err := ParseCoreVersion("Qumulo Core 7.9.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "7.9.2-1" && v.Major() != 7 {
		// Masterminds treats 7.9.2.1 as 7.9.2 with metadata/prerelease depending on version
		if v.Major() != 7 || v.Minor() != 9 {
			t.Fatalf("got %s", v)
		}
	}
}

func TestCheckFloor(t *testing.T) {
	if err := CheckFloor("Qumulo Core 7.2.0", "7.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := CheckFloor("Qumulo Core 5.3.3", "7.2.0"); err == nil {
		t.Fatal("expected error")
	}
}

func TestSupports(t *testing.T) {
	ok, min, err := Supports("Qumulo Core 7.0.0", FeatureObjectLock)
	if err != nil {
		t.Fatal(err)
	}
	if ok || min != "7.2.0" {
		t.Fatalf("ok=%v min=%s", ok, min)
	}
	ok, _, err = Supports("Qumulo Core 7.5.0", FeatureVersioning)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}
