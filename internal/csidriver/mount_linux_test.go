//go:build linux

package csidriver

import "testing"

func TestParseMountInfoSelectsVisibleTopmostMount(t *testing.T) {
	const target = "/var/lib/kubelet/pods/pod-a/volumes/target"
	raw := []byte(
		"386 82 0:100 /sub " + target + " rw,relatime - nfs4 q.example:/exports/a rw,vers=4.1,sec=sys,hard,timeo=600,retrans=2,nodev,nosuid\n" +
			"387 386 0:100 / " + target + " ro,nodev,nosuid - nfs4 q.example:/exports/b rw,vers=4.1,sec=sys,hard,timeo=600,retrans=2\n",
	)

	record, mounted, err := parseMountInfo(raw, target)
	if err != nil {
		t.Fatal(err)
	}
	if !mounted {
		t.Fatal("stacked target was not found")
	}
	if record.MountID != "387" || record.ParentID != "386" || record.Root != "/" || record.Source != "q.example:/exports/b" {
		t.Fatalf("selected wrong mount record: %#v", record)
	}
}

func TestParseMountInfoPreservesSubtreeRoot(t *testing.T) {
	const target = "/var/lib/kubelet/pods/pod-a/volumes/target"
	raw := []byte("386 82 0:100 /sub\\040dir " + target + " rw,nodev,nosuid - cifs //q.example/share rw,vers=3.1.1,seal\n")

	record, mounted, err := parseMountInfo(raw, target)
	if err != nil || !mounted {
		t.Fatalf("mounted=%v err=%v", mounted, err)
	}
	if record.Root != "/sub dir" {
		t.Fatalf("root=%q", record.Root)
	}
}
