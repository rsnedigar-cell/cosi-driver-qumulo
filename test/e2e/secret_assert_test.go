package e2e

import (
	"encoding/json"
	"testing"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/naming"
)

// BucketInfo is the document the COSI sidecar writes into the credentials Secret.
// Field names are locked against the v0.2.2 sidecar (SecretS3 / BucketInfo).
type BucketInfo struct {
	Spec struct {
		BucketName         string   `json:"bucketName"`
		AuthenticationType string   `json:"authenticationType"`
		SecretS3           SecretS3 `json:"secretS3"`
		Protocols          []string `json:"protocols"`
	} `json:"spec"`
}

type SecretS3 struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	AccessKeyID     string `json:"accessKeyID"`
	AccessSecretKey string `json:"accessSecretKey"`
}

func TestBucketInfoKeyContract(t *testing.T) {
	// The driver emits these exact keys; the sidecar copies them onto SecretS3.
	want := []string{
		naming.SecretAccessKeyID,
		naming.SecretAccessSecretKey,
		naming.SecretEndpoint,
		naming.SecretRegion,
	}
	raw := map[string]string{
		naming.SecretAccessKeyID:     "AKIAEXAMPLE",
		naming.SecretAccessSecretKey: "secret",
		naming.SecretEndpoint:        "https://qumulo.example:9000",
		naming.SecretRegion:          "us-east-1",
	}
	doc := BucketInfo{}
	doc.Spec.BucketName = "photos"
	doc.Spec.AuthenticationType = "KEY"
	doc.Spec.SecretS3 = SecretS3{
		Endpoint:        raw[naming.SecretEndpoint],
		Region:          raw[naming.SecretRegion],
		AccessKeyID:     raw[naming.SecretAccessKeyID],
		AccessSecretKey: raw[naming.SecretAccessSecretKey],
	}
	doc.Spec.Protocols = []string{"s3"}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var round BucketInfo
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
	if round.Spec.SecretS3.AccessKeyID != "AKIAEXAMPLE" {
		t.Fatalf("sidecar key contract drifted: %s", b)
	}
	for _, k := range want {
		if raw[k] == "" {
			t.Fatalf("missing %s", k)
		}
	}
}
