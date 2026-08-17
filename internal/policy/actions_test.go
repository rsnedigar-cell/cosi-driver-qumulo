package policy

import (
	"strings"
	"testing"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

func TestNormalizeMode(t *testing.T) {
	cases := map[string]string{
		"": "rw", "rw": "rw", "ro": "ro", "full": "full", "ReadOnly": "ro",
	}
	for in, want := range cases {
		got, err := NormalizeMode(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
	if _, err := NormalizeMode("write-only"); err == nil {
		t.Fatal("expected error")
	}
}

func TestUpsertAndRemove(t *testing.T) {
	p := qumulo.EmptyPolicy()
	if err := UpsertStatement(p, "cosi-alice", "alice", "ro", "photos"); err != nil {
		t.Fatal(err)
	}
	if err := UpsertStatement(p, "cosi-bob", "bob", "rw", "photos"); err != nil {
		t.Fatal(err)
	}
	if len(p.Statement) != 2 {
		t.Fatalf("len=%d", len(p.Statement))
	}
	// upgrade alice to full (replace in place)
	if err := UpsertStatement(p, "cosi-alice", "alice", "full", "photos"); err != nil {
		t.Fatal(err)
	}
	if len(p.Statement) != 2 {
		t.Fatalf("upsert grew list: %d", len(p.Statement))
	}
	var alice qumulo.PolicyStatement
	for _, s := range p.Statement {
		if s.Sid == "cosi-alice" {
			alice = s
		}
	}
	acts, _ := alice.Action.([]string)
	if !EqualActions(acts, Actions[ModeFull]) {
		t.Fatalf("alice actions=%v", alice.Action)
	}
	if alice.Resource != nil {
		t.Fatalf("Resource must be omitted (Core 7.9.2 ResourceSpecified): %#v", alice.Resource)
	}
	// Principals must be auth_id-based: name-based principals canonicalize
	// against removed identities on Core 7.9.2.2 and match nobody.
	if !strings.Contains(string(alice.Principal), `"auth_id:alice"`) {
		t.Fatalf("principal must use auth_id form: %s", alice.Principal)
	}
	RemoveStatement(p, "cosi-bob")
	if len(p.Statement) != 1 || p.Statement[0].Sid != "cosi-alice" {
		t.Fatalf("remove failed: %+v", p.Statement)
	}
	RemoveStatement(p, "missing")
	if len(p.Statement) != 1 {
		t.Fatal("remove missing changed list")
	}
}

func TestROIsSubsetOfRW(t *testing.T) {
	ro := map[string]struct{}{}
	for _, a := range Actions[ModeRO] {
		ro[a] = struct{}{}
	}
	for _, a := range Actions[ModeRO] {
		found := false
		for _, b := range Actions[ModeRW] {
			if a == b {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("rw missing ro action %s", a)
		}
	}
}

func TestRemoveStatementForAuthIDPreservesReplacementIdentity(t *testing.T) {
	p := qumulo.EmptyPolicy()
	if err := UpsertStatement(p, "cosi-access", "new-auth-id", "rw", "bucket"); err != nil {
		t.Fatal(err)
	}
	RemoveStatementForAuthID(p, "cosi-access", "old-auth-id")
	if len(p.Statement) != 1 {
		t.Fatal("stale revoke removed replacement identity's statement")
	}
	RemoveStatementForAuthID(p, "cosi-access", "new-auth-id")
	if len(p.Statement) != 0 {
		t.Fatal("matching revoke did not remove statement")
	}
}
