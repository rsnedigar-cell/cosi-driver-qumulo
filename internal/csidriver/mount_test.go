package csidriver

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSanitizedMountFlagsRejectSecretsAndBind(t *testing.T) {
	for _, flag := range []string{"password=secret", "credentials=/tmp/x", "bind", "rw,password=x"} {
		if _, err := sanitizedMountFlags([]string{flag}); err == nil {
			t.Fatalf("expected %q to be rejected", flag)
		}
	}
	got, err := sanitizedMountFlags([]string{"hard,timeo=600", "noatime"})
	if err != nil || len(got) != 3 {
		t.Fatalf("safe flags: %v, %v", got, err)
	}
}

func TestSanitizedMountFlagsRejectRedirectAndAuthenticationAliases(t *testing.T) {
	blocked := []string{
		"addr=192.0.2.10", "ip=192.0.2.10", "mountaddr=192.0.2.10", "mounthost=evil.example",
		"cred=/tmp/attacker", "CREDS=/tmp/attacker", "credential=/tmp/attacker",
		"credentials=/tmp/attacker", "credfile=/tmp/attacker", "credentialsfile=/tmp/attacker",
		"user=attacker", "username=attacker", "user", "username",
		"pass=secret", "password=secret", "pass2=secret", "password2=secret",
		"domain=EVIL", "dom=EVIL", "workgroup=EVIL", "domainauto",
		"unc=//evil/share", "source=//evil/share", "prefixpath=other", "prepath=other",
		"vers=1.0", "version=1.0", "nfsvers=3", "mountvers=3", "minorversion=0",
		"sec=none", "security=none", "seal", "noseal", "sign", "nosign",
		"proto=udp", "protocol=udp", "mountproto=udp", "tcp", "tcp6", "udp", "udp6", "rdma", "rdma6",
		"soft", "softerr", "softreval", "nosoftreval", "port=445", "mountport=2049",
		"guest", "nullauth", "multiuser", "cruid=1000", "upcall_target=app",
		"backupuid=0", "backupgid=0", "backup_uid=0", "backup_gid=0", "snapshot=@GMT-2026.08.17-12.00.00",
		"sharesock", "nosharesock", "sloppy", "bg", "background", "nofail",
		"users", "owner", "group", "ro", "rw",
		"X-mount.subdir=other", "x-mount.nocanonicalize=target", "X-mount.owner=1000",
		"X-mount.group=1000", "X-mount.mode=0777", "X-mount.idmap=/proc/1/ns/user",
		"user =attacker", "rw,username=attacker", "--bind",
	}
	for _, flag := range blocked {
		t.Run(flag, func(t *testing.T) {
			if _, err := sanitizedMountFlags([]string{flag}); err == nil {
				t.Fatalf("expected %q to be rejected", flag)
			}
		})
	}
}

func TestSanitizedMountFlagsRejectConflictingDuplicatesAndFlags(t *testing.T) {
	for _, flags := range [][]string{
		{"rsize=65536", "rsize=1048576"},
		{"cache=strict,cache=none"},
		{"exec", "noexec"},
		{"atime,noatime"},
		{"sync", "async"},
		{"ro", "rw"},
	} {
		if _, err := sanitizedMountFlags(flags); err == nil {
			t.Fatalf("conflicting options accepted: %v", flags)
		}
	}
	got, err := sanitizedMountFlags([]string{"rsize=1048576", "RSIZE=1048576", "noexec", "noexec"})
	if err != nil {
		t.Fatalf("identical duplicate options rejected: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("identical options were not deduplicated: %v", got)
	}
}

func TestSanitizedMountFlagsPreserveSafeOperationalOptions(t *testing.T) {
	flags := []string{
		"rsize=1048576,wsize=1048576", "noatime", "nodiratime",
		"lookupcache=positive", "actimeo=30", "cache=strict",
		"uid=1000", "gid=1000", "file_mode=0640", "dir_mode=0750",
		"noserverino", "hard", "nodev", "nosuid",
	}
	got, err := sanitizedMountFlags(flags)
	if err != nil {
		t.Fatalf("safe operational flags rejected: %v", err)
	}
	if len(got) != 15 {
		t.Fatalf("got %d safe flags, want 15: %v", len(got), got)
	}
}

func TestRejectControlledMountOptionsRejectsBundledAndMalformedKeys(t *testing.T) {
	for _, flags := range [][]string{{"noatime,timeo=50"}, {"retrans=4"}, {"timeo =50"}} {
		if err := rejectControlledMountOptions(flags, "timeo", "retrans"); err == nil {
			t.Fatalf("expected %q to be rejected", flags)
		}
	}
	if err := rejectControlledMountOptions([]string{"noatime", "rsize=1048576"}, "timeo", "retrans"); err != nil {
		t.Fatalf("safe options rejected: %v", err)
	}
}

func TestValidateExistingMountChecksModeTypeAndSecurity(t *testing.T) {
	h := volumeHandle{Protocol: protocolNFS, Server: "q.example", ResourceName: "/exports/a", NFSVersion: "4.1"}
	valid := mountRecord{
		Root: "/", Source: "q.example:/exports/a", FSType: "nfs4",
		Options: []string{"rw", "vers=4.1", "sec=sys", "hard", "timeo=600", "retrans=2", "nodev", "nosuid"},
	}
	if err := validateExistingMount(h, valid, false, nil); err != nil {
		t.Fatalf("valid mount rejected: %v", err)
	}
	readOnlyView := valid
	readOnlyView.MountOptions = []string{"ro", "nodev", "nosuid"}
	readOnlyView.Options = append([]string{"rw"}, valid.Options[1:]...)
	if err := validateExistingMount(h, readOnlyView, true, nil); err != nil {
		t.Fatalf("read-only mount view rejected because its superblock is writable: %v", err)
	}

	tests := []struct {
		name     string
		record   mountRecord
		readOnly bool
	}{
		{name: "read-only retry against read-write mount", record: valid, readOnly: true},
		{name: "filesystem type", record: mountRecord{Root: "/", Source: valid.Source, FSType: "nfs", Options: valid.Options}},
		{name: "filesystem subtree", record: mountRecord{Root: "/sub", Source: valid.Source, FSType: "nfs4", Options: valid.Options}},
		{name: "version", record: mountRecord{Root: "/", Source: valid.Source, FSType: "nfs4", Options: []string{"rw", "vers=4.0", "sec=sys", "hard", "timeo=600", "retrans=2", "nodev", "nosuid"}}},
		{name: "security flavor", record: mountRecord{Root: "/", Source: valid.Source, FSType: "nfs4", Options: []string{"rw", "vers=4.1", "sec=krb5", "hard", "timeo=600", "retrans=2", "nodev", "nosuid"}}},
		{name: "soft retry policy", record: mountRecord{Root: "/", Source: valid.Source, FSType: "nfs4", Options: []string{"rw", "vers=4.1", "sec=sys", "soft", "timeo=600", "retrans=2", "nodev", "nosuid"}}},
		{name: "missing nodev", record: mountRecord{Root: "/", Source: valid.Source, FSType: "nfs4", Options: []string{"rw", "vers=4.1", "sec=sys", "hard", "timeo=600", "retrans=2", "nosuid"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateExistingMount(h, test.record, test.readOnly, nil); err == nil {
				t.Fatal("expected incompatible mount to be rejected")
			}
		})
	}
}

func TestValidateExistingEncryptedSMBMount(t *testing.T) {
	h := volumeHandle{Protocol: protocolSMB, Server: "q.example", ResourceName: "share", SMBEncrypted: true}
	record := mountRecord{Root: "/", Source: "//q.example/share", FSType: "cifs", Options: []string{"ro", "vers=3.1.1", "seal", "nodev", "nosuid"}}
	if err := validateExistingMount(h, record, true, nil); err != nil {
		t.Fatalf("valid encrypted SMB mount rejected: %v", err)
	}
	record.Options = []string{"ro", "vers=3.1.1", "nodev", "nosuid"}
	if err := validateExistingMount(h, record, true, nil); err == nil {
		t.Fatal("unencrypted SMB mount accepted for encrypted handle")
	}
}

func TestValidateExistingMountChecksRequestedOperationalOptions(t *testing.T) {
	h := volumeHandle{Protocol: protocolNFS, Server: "q.example", ResourceName: "/exports/a", NFSVersion: "4.1"}
	record := mountRecord{
		Root: "/", Source: mountSource(h), FSType: "nfs4",
		Options: []string{"rw", "vers=4.1", "sec=sys", "hard", "timeo=600", "retrans=2", "nodev", "nosuid", "noexec", "rsize=1048576"},
	}
	if err := validateExistingMount(h, record, false, []string{"noexec", "rsize=1048576"}); err != nil {
		t.Fatalf("matching requested options rejected: %v", err)
	}
	if err := validateExistingMount(h, record, false, []string{"rsize=65536"}); err == nil {
		t.Fatal("incompatible requested option value accepted")
	}
	if err := validateExistingMount(h, record, false, []string{"nodiratime"}); err == nil {
		t.Fatal("missing requested option accepted")
	}
}

func TestValidateTargetPathRejectsEscapeAndSymlink(t *testing.T) {
	root := t.TempDir()
	if err := validateTargetPath(root, filepath.Join(root, "pods", "a")); err != nil {
		t.Fatal(err)
	}
	if err := validateTargetPath(root, filepath.Join(root, "..", "escape")); err == nil {
		t.Fatal("expected path escape rejection")
	}
	if err := validateTargetPath(root, filepath.Join("pods", "relative")); err == nil {
		t.Fatal("expected relative target rejection")
	}
	if err := os.Mkdir(filepath.Join(root, "pods"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "pods", "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := validateTargetPath(root, filepath.Join(root, "pods", "link", "target")); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestWriteSMBCredentialsUsesFileWithoutLoggingValues(t *testing.T) {
	path, cleanup, err := writeSMBCredentials(t.TempDir(), map[string]string{
		"username": "alice", "password": "s3cr3t,with,commas", "domain": "EXAMPLE",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "password=s3cr3t,with,commas") {
		t.Fatalf("credentials file missing password")
	}
	info, _ := os.Stat(path)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode = %o", info.Mode().Perm())
	}
}
