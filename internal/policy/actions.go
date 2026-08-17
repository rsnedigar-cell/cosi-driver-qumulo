// Package policy maps COSI accessMode values to Qumulo S3 policy actions
// and merges statement lists idempotently.
package policy

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

const (
	ModeRO   = "ro"
	ModeRW   = "rw"
	ModeFull = "full"
)

// Actions is the single source of truth for accessMode → S3 action sets.
// Reconciled against Qumulo's documented S3 policy action set.
var Actions = map[string][]string{
	ModeRO: {
		"s3:GetObject",
		"s3:GetObjectAttributes",
		"s3:ListBucket",
		"s3:GetBucketLocation",
	},
	ModeRW: {
		"s3:GetObject",
		"s3:GetObjectAttributes",
		"s3:ListBucket",
		"s3:GetBucketLocation",
		"s3:PutObject",
		"s3:DeleteObject",
		"s3:AbortMultipartUpload",
		"s3:ListBucketMultipartUploads",
		"s3:ListMultipartUploadParts",
	},
	ModeFull: {
		"s3:*",
	},
}

func NormalizeMode(m string) (string, error) {
	switch m {
	case "", ModeRW, "readwrite", "ReadWrite":
		return ModeRW, nil
	case ModeRO, "readonly", "ReadOnly":
		return ModeRO, nil
	case ModeFull, "*", "admin":
		return ModeFull, nil
	default:
		return "", fmt.Errorf("unknown accessMode %q (want ro, rw, or full)", m)
	}
}

func ActionsFor(mode string) ([]string, error) {
	m, err := NormalizeMode(mode)
	if err != nil {
		return nil, err
	}
	out := append([]string(nil), Actions[m]...)
	return out, nil
}

// UpsertStatement inserts or replaces the statement identified by sid.
// authID is the grantee's numeric auth id — principals must never be
// name-based (see qumulo.QumuloPrincipal).
func UpsertStatement(p *qumulo.Policy, sid, authID, mode, _ string) error {
	actions, err := ActionsFor(mode)
	if err != nil {
		return err
	}
	// Core 7.9.2.2 rejects Resource with ResourceSpecified. The live
	// private-bucket default is Principal + Action only.
	stmt := qumulo.PolicyStatement{
		Sid:       sid,
		Effect:    "Allow",
		Principal: qumulo.QumuloPrincipal(authID),
		Action:    actions,
	}
	if p.Version == "" {
		p.Version = "2012-10-17"
	}
	for i := range p.Statement {
		if p.Statement[i].Sid == sid {
			p.Statement[i] = stmt
			return nil
		}
	}
	p.Statement = append(p.Statement, stmt)
	return nil
}

// RemoveStatement drops the statement with sid. Missing is a no-op.
func RemoveStatement(p *qumulo.Policy, sid string) {
	if p == nil {
		return
	}
	dst := p.Statement[:0]
	for _, s := range p.Statement {
		if s.Sid == sid {
			continue
		}
		dst = append(dst, s)
	}
	p.Statement = dst
}

// RemoveStatementForAuthID drops sid only when it still belongs to authID.
// A local username may be deleted and recreated with a new immutable auth id;
// a delayed revoke for the old account must not remove the replacement
// identity's policy statement. An empty authID preserves q1 compatibility.
func RemoveStatementForAuthID(p *qumulo.Policy, sid, authID string) {
	if p == nil {
		return
	}
	dst := p.Statement[:0]
	for _, s := range p.Statement {
		if s.Sid != sid || (authID != "" && !principalHasAuthID(s.Principal, authID)) {
			dst = append(dst, s)
		}
	}
	p.Statement = dst
}

func principalHasAuthID(raw json.RawMessage, authID string) bool {
	var principal map[string]json.RawMessage
	if err := json.Unmarshal(raw, &principal); err != nil {
		return false
	}
	want := "auth_id:" + authID
	var values []string
	if err := json.Unmarshal(principal["Qumulo"], &values); err != nil {
		var one string
		if err := json.Unmarshal(principal["Qumulo"], &one); err != nil {
			return false
		}
		values = []string{one}
	}
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// EqualActions is a test helper.
func EqualActions(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// StatementSID reports the Sid at i (test helper).
func StatementJSON(s qumulo.PolicyStatement) string {
	b, _ := json.Marshal(s)
	return string(b)
}
