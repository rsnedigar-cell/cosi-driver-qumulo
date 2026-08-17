package qumulo

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestEncodeFSPath(t *testing.T) {
	cases := map[string]string{
		"":                    "%2F",
		"/":                   "%2F",
		"/cosi-live-probe":    "%2Fcosi-live-probe",
		"cosi-live-probe":     "%2Fcosi-live-probe",
		"/k8s-buckets/photos": "%2Fk8s-buckets%2Fphotos",
		"k8s-buckets/photos":  "%2Fk8s-buckets%2Fphotos",
	}
	for in, want := range cases {
		if got := encodeFSPath(in); got != want {
			t.Fatalf("encodeFSPath(%q)=%q want %q", in, got, want)
		}
	}
}

func TestGrantDirectoryAccessRetriesETagConflict(t *testing.T) {
	acl := ACL{Control: []string{"PRESENT"}, ACES: []ACE{{Type: "ALLOWED", Flags: []string{}, Trustee: "existing", Rights: []string{"READ"}}}}
	etag := `"1"`
	putCalls := 0
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set(headerETag, etag)
			writeProtocolJSON(t, w, http.StatusOK, map[string]any{"generated": false, "acl": acl})
		case http.MethodPut:
			putCalls++
			if putCalls == 1 {
				// Simulate another writer committing between our GET and PUT.
				acl.ACES = append(acl.ACES, ACE{Type: "ALLOWED", Flags: []string{}, Trustee: "concurrent", Rights: []string{"READ"}})
				etag = `"2"`
				writeProtocolJSON(t, w, http.StatusPreconditionFailed, APIError{ErrorClass: ErrClassRESTPrecondition})
				return
			}
			if got := r.Header.Get(headerIfMatch); got != etag {
				t.Errorf("If-Match=%q want %q", got, etag)
			}
			if err := json.NewDecoder(r.Body).Decode(&acl); err != nil {
				t.Errorf("decode ACL: %v", err)
			}
			writeProtocolJSON(t, w, http.StatusOK, nil)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	if _, err := conn.GrantDirectoryAccess(context.Background(), "/bucket", "new-user", "rw", false); err != nil {
		t.Fatal(err)
	}
	for _, trustee := range []string{"existing", "concurrent", "new-user"} {
		if !aclHasTrustee(&acl, trustee) {
			t.Fatalf("trustee %q was lost: %#v", trustee, acl.ACES)
		}
	}
}

func TestGrantDirectoryAccessRejectsChmodFallback(t *testing.T) {
	conn := &Connection{}
	if _, err := conn.GrantDirectoryAccess(context.Background(), "/bucket", "user", "rw", true); err == nil {
		t.Fatal("world-writable chmod fallback was accepted")
	}
}

func TestGrantDirectoryAccessReconcilesExactManagedACE(t *testing.T) {
	acl := ACL{Control: []string{"PRESENT"}, ACES: []ACE{
		{Type: "ALLOWED", Flags: []string{}, Trustee: "other", Rights: []string{"READ"}},
		{Type: "DENIED", Flags: []string{}, Trustee: "managed", Rights: append([]string(nil), fsRightsRW...)},
	}}
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set(headerETag, `"acl-1"`)
			writeProtocolJSON(t, w, http.StatusOK, map[string]any{"acl": acl})
		case http.MethodPut:
			if got := r.Header.Get(headerIfMatch); got != `"acl-1"` {
				t.Errorf("If-Match=%q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&acl); err != nil {
				t.Errorf("decode ACL: %v", err)
			}
			writeProtocolJSON(t, w, http.StatusOK, nil)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	if _, err := conn.GrantDirectoryAccess(context.Background(), "/bucket", "managed", "ro", false); err != nil {
		t.Fatal(err)
	}
	wantRO := ACE{Type: "ALLOWED", Flags: fsInherit, Trustee: "managed", Rights: fsRightsRO}
	if !aclHasExactTrusteeGrant(&acl, wantRO) || !aclHasTrustee(&acl, "other") {
		t.Fatalf("RO reconciliation produced %#v", acl.ACES)
	}
	if _, err := conn.GrantDirectoryAccess(context.Background(), "/bucket", "managed", "rw", false); err != nil {
		t.Fatal(err)
	}
	wantRW := ACE{Type: "ALLOWED", Flags: fsInherit, Trustee: "managed", Rights: fsRightsRW}
	if !aclHasExactTrusteeGrant(&acl, wantRW) || !aclHasTrustee(&acl, "other") {
		t.Fatalf("RW reconciliation produced %#v", acl.ACES)
	}
}
