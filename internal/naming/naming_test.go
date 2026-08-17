package naming

import (
	"strings"
	"testing"
)

func TestBucketName_Passthrough(t *testing.T) {
	got, err := BucketName("", "photos-prod")
	if err != nil {
		t.Fatal(err)
	}
	if got != "photos-prod" {
		t.Fatalf("got %q", got)
	}
}

func TestBucketName_SanitizeAndHash(t *testing.T) {
	got, err := BucketName("team-", "My.Bucket_Name")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateBucketName(got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "team-") {
		t.Fatalf("missing prefix: %q", got)
	}
	// hash suffix required because sanitization changed the input
	if !strings.Contains(got[len("team-"):], "-") {
		t.Fatalf("expected hash suffix, got %q", got)
	}
	again, err := BucketName("team-", "My.Bucket_Name")
	if err != nil {
		t.Fatal(err)
	}
	if again != got {
		t.Fatalf("not deterministic: %q vs %q", got, again)
	}
}

func TestBucketName_Clamp63(t *testing.T) {
	long := strings.Repeat("a", 80)
	got, err := BucketName("pfx-", long)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 63 {
		t.Fatalf("len=%d want 63 (%q)", len(got), got)
	}
	if err := ValidateBucketName(got); err != nil {
		t.Fatal(err)
	}
}

func TestBucketNameResistsFormer32BitCollision(t *testing.T) {
	left, err := BucketName("", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa51920")
	if err != nil {
		t.Fatal(err)
	}
	right, err := BucketName("", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa106464")
	if err != nil {
		t.Fatal(err)
	}
	if left == right {
		t.Fatalf("distinct COSI names collapsed to bucket %q", left)
	}
}

func TestValidateBucketName(t *testing.T) {
	cases := []struct {
		name    string
		wantErr bool
	}{
		{"abc", false},
		{"a", true},
		{"ab", true},
		{"-abc", true},
		{"abc-", true},
		{"ab.c", true},
		{"ABC", true},
		{"has_underscore", true},
		{strings.Repeat("a", 63), false},
		{strings.Repeat("a", 64), true},
	}
	for _, tc := range cases {
		err := ValidateBucketName(tc.name)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateBucketName(%q) err=%v wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}

func TestUsername_DeterministicAndPrefixed(t *testing.T) {
	u1 := Username("q1:qumulo:host:8000:photos", "photos-access")
	u2 := Username("q1:qumulo:host:8000:photos", "photos-access")
	u3 := Username("q1:qumulo:host:8000:photos", "other")
	if u1 != u2 {
		t.Fatal("not deterministic")
	}
	if u1 == u3 {
		t.Fatal("collision across access names")
	}
	if !IsDriverUser(u1) {
		t.Fatalf("expected driver user %q", u1)
	}
	if len(strings.TrimPrefix(u1, userPrefix)) != 32 {
		t.Fatalf("new driver username lacks 128-bit suffix: %q", u1)
	}
	if !IsDriverUser("cosi-0123456789ab") {
		t.Fatal("legacy driver username must remain revocable")
	}
	if IsDriverUser("admin") || IsDriverUser("cosi-nothexzzzz") {
		t.Fatal("false positive")
	}
}

func TestBucketIDRoundTrip(t *testing.T) {
	id := BucketID{Endpoint: "qumulo.example.com", RESTPort: "8000", BucketName: "team-photos"}
	parsed, err := ParseBucketID(id.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != id {
		t.Fatalf("%+v != %+v", parsed, id)
	}
}

func TestBucketIDV2RoundTrip(t *testing.T) {
	id := BucketID{
		Endpoint:   "qumulo.example.com",
		RESTPort:   "8000",
		BucketName: "team-photos",
		RootPath:   "/k8s buckets/team-photos",
		RootFileID: "123:456",
	}
	if got := id.String(); !strings.HasPrefix(got, BucketIDVersion+":qumulo:") {
		t.Fatalf("new bucket ID did not use %s: %q", BucketIDVersion, got)
	}
	parsed, err := ParseBucketID(id.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != id {
		t.Fatalf("%+v != %+v", parsed, id)
	}
}

func TestManagedBucketIDRoundTrip(t *testing.T) {
	id := BucketID{
		Endpoint:   "qumulo.example.com",
		RESTPort:   "8000",
		BucketName: "team-photos",
		RootPath:   "/k8s buckets/team-photos-fingerprint",
		RootFileID: "123:456",
		Managed:    true,
	}
	if got := id.String(); !strings.HasPrefix(got, ManagedBucketIDVersion+":qumulo:") {
		t.Fatalf("managed bucket ID did not use %s: %q", ManagedBucketIDVersion, got)
	}
	parsed, err := ParseBucketID(id.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != id {
		t.Fatalf("%+v != %+v", parsed, id)
	}
}

func TestParseBucketID_Rejects(t *testing.T) {
	for _, raw := range []string{"", "foo", "q1:nfs:h:8000:b", "q1:qumulo::8000:b", "q2:qumulo:h:8000:b:relative:id", "q2:qumulo:h:8000:b:/root:", "q3:qumulo:h:8000:b:/root:id:foreign"} {
		if _, err := ParseBucketID(raw); err == nil {
			t.Errorf("expected error for %q", raw)
		}
	}
}

func TestAccountIDRoundTrip(t *testing.T) {
	id := AccountID{Endpoint: "q.example", Username: "cosi-abcdef012345", AccessKeyPfx: "AKIA1234"}
	parsed, err := ParseAccountID(id.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != id {
		t.Fatalf("%+v != %+v", parsed, id)
	}
}

func TestAccountIDV2RoundTrip(t *testing.T) {
	id := AccountID{
		Endpoint:     "q.example",
		Username:     "cosi-abcdef012345",
		AccessKeyPfx: "AKIA1234",
		AuthID:       "123:456",
	}
	if got := id.String(); !strings.HasPrefix(got, AccountIDAuthVersion+":qumulo:") {
		t.Fatalf("new account ID did not use %s: %q", AccountIDAuthVersion, got)
	}
	parsed, err := ParseAccountID(id.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != id {
		t.Fatalf("%+v != %+v", parsed, id)
	}
}

func TestAccountIDV3RestoreModeRoundTrip(t *testing.T) {
	id := AccountID{
		Endpoint: "q.example", Username: "cosi-abcdef012345", AccessKeyPfx: "AKIA1234",
		AuthID: "123:456", RestoreMode: "0750",
	}
	if got := id.String(); !strings.HasPrefix(got, AccountIDVersion+":qumulo:") {
		t.Fatalf("fallback account ID did not use %s: %q", AccountIDVersion, got)
	}
	parsed, err := ParseAccountID(id.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != id {
		t.Fatalf("%+v != %+v", parsed, id)
	}
	if _, err := ParseAccountID("q3:qumulo:q.example:user:key:auth:0999"); err == nil {
		t.Fatal("invalid restore mode was accepted")
	}
}

func TestS3Endpoint(t *testing.T) {
	got := S3Endpoint("https://qumulo.lab:8000", "")
	if got != "https://qumulo.lab:9000" {
		t.Fatalf("got %q", got)
	}
	got = S3Endpoint("qumulo.lab", "9443")
	if got != "https://qumulo.lab:9443" {
		t.Fatalf("got %q", got)
	}
}

func TestRESTBaseURL(t *testing.T) {
	got := RESTBaseURL("qumulo.lab", "8000")
	if got != "https://qumulo.lab:8000" {
		t.Fatalf("got %q", got)
	}
}

func TestRootPath(t *testing.T) {
	if got := RootPath("/k8s-buckets", "photos"); got != "/k8s-buckets/photos" {
		t.Fatalf("got %q", got)
	}
	if got := RootPath("/", "photos"); got != "/photos" {
		t.Fatalf("got %q", got)
	}
}

func TestAccessKeyPrefix(t *testing.T) {
	if got := AccessKeyPrefix("ABCDEFGH1234"); got != "ABCDEFGH" {
		t.Fatalf("got %q", got)
	}
	if got := AccessKeyPrefix("short"); got != "short" {
		t.Fatalf("got %q", got)
	}
}
