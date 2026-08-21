package qumulo

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
)

const (
	// AbsoluteMinimum is S3 API GA.
	AbsoluteMinimum = "5.3.3"
	// DefaultFloor enables policies, versioning, private-at-create, lock.
	DefaultFloor = "7.2.0"

	FeaturePrivateBucket = "private-bucket"
	FeatureVersioning    = "versioning"
	FeatureObjectLock    = "object-lock"
	FeatureBucketPolicy  = "bucket-policy"
	FeatureQuota         = "quota"
	FeatureTreeDelete    = "tree-delete"
	FeatureSnapshots     = "directory-snapshots"
)

// FeatureMinima is the introducing Core version per feature. Kept as a
// single table so a later confirmation against the official
// supported-functionality matrix is a one-file change.
var FeatureMinima = map[string]string{
	FeaturePrivateBucket: "7.2.0",
	FeatureVersioning:    "7.1.0",
	FeatureObjectLock:    "7.2.0",
	FeatureBucketPolicy:  "7.1.0",
	FeatureQuota:         "5.3.3",
	FeatureTreeDelete:    "5.3.3",
	// Directory snapshots predate the driver's 7.2 floor. The v3 REST
	// surface is pending a live lock against Core 7.9.2.2.
	FeatureSnapshots: "5.3.3",
}

var coreVersionRE = regexp.MustCompile(`(?i)Qumulo Core\s+([0-9]+(?:\.[0-9]+){1,3})`)

// ParseCoreVersion extracts a semver from `GET /v1/version` revision_id.
func ParseCoreVersion(revisionID string) (*semver.Version, error) {
	s := strings.TrimSpace(revisionID)
	if m := coreVersionRE.FindStringSubmatch(s); len(m) == 2 {
		s = m[1]
	}
	s = strings.TrimPrefix(s, "v")
	// Qumulo sometimes reports four-part revisions (7.9.2.1). Semver is
	// major.minor.patch — keep the first three and stash the rest as metadata.
	if parts := strings.SplitN(s, ".", 4); len(parts) == 4 {
		s = parts[0] + "." + parts[1] + "." + parts[2] + "+" + parts[3]
	}
	v, err := semver.NewVersion(s)
	if err != nil {
		return nil, fmt.Errorf("parse Qumulo Core version %q: %w", revisionID, err)
	}
	return v, nil
}

// CheckFloor returns an error if cluster is older than floor (or the
// absolute S3 GA minimum, whichever is higher).
func CheckFloor(cluster, floor string) error {
	cv, err := ParseCoreVersion(cluster)
	if err != nil {
		return err
	}
	want := floor
	if want == "" {
		want = DefaultFloor
	}
	fv, err := semver.NewVersion(strings.TrimPrefix(want, "v"))
	if err != nil {
		return fmt.Errorf("invalid version floor %q: %w", want, err)
	}
	abs, _ := semver.NewVersion(AbsoluteMinimum)
	if fv.LessThan(abs) {
		fv = abs
	}
	if cv.LessThan(fv) {
		return fmt.Errorf("Qumulo Core %s is below the driver floor %s (S3 GA is %s)", cv, fv, AbsoluteMinimum)
	}
	return nil
}

// Supports reports whether cluster meets the introducing version for feature.
func Supports(cluster, feature string) (bool, string, error) {
	min, ok := FeatureMinima[feature]
	if !ok {
		return false, "", fmt.Errorf("unknown feature %q", feature)
	}
	cv, err := ParseCoreVersion(cluster)
	if err != nil {
		return false, min, err
	}
	mv, err := semver.NewVersion(min)
	if err != nil {
		return false, min, err
	}
	return !cv.LessThan(mv), min, nil
}
