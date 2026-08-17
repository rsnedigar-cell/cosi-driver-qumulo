// Package fakequmulo is an httptest server covering the Qumulo REST surface
// the COSI driver uses, including error envelopes, ETag conflicts, the
// 2-key limit, and once-only secret display.
package fakequmulo

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

type Server struct {
	*httptest.Server
	mu sync.Mutex

	Version     string
	S3Enabled   bool
	S3BasePath  string
	RequireAuth bool
	Token       string

	Buckets  map[string]*bucketState
	Users    map[string]*userState
	Quota    map[string]int64
	Files    map[string]string // path → file id
	ACLs     map[string]*qumulo.ACL
	Modes    map[string]string
	TreeJobs []string // submitted job history

	// TreeDeletePolls controls how many RUNNING responses a newly submitted
	// job returns before GET reports the documented 404 completion signal.
	TreeDeletePolls int
	// TreeDeleteError makes newly submitted jobs abort with this message.
	TreeDeleteError string
	treeDeleteJobs  map[string]*treeDeleteState

	PolicyConflictN atomic.Int32 // remaining forced 412s on policy PUT
	HideListN       atomic.Int32 // bucket listings to serve empty (miss injection)
	FailTreeDeleteN atomic.Int32 // tree-delete requests to fail before queuing
	UserSeq         atomic.Int64 // unique auth id across same-name recreation
	FailNext        string       // error_class to inject on next mutating call
	FailACLGet      bool         // inject a 500 on GET /info/acl
	FailACLPut      bool         // inject a 400 on PUT /info/acl
	FailPolicyPut   bool         // inject a 400 on bucket policy PUT
	RejectPrivate   bool         // legacy Core: reject create with "private" field
}

type bucketState struct {
	qumulo.Bucket
	Policy     *qumulo.Policy
	ETag       int
	Uploads    map[string]qumulo.MultipartUpload
	NotEmpty   bool
	HasUploads bool
}

type userState struct {
	qumulo.LocalUser
	Password string
	Keys     []storedKey
}

type storedKey struct {
	qumulo.AccessKey
	secretShown bool
}

type treeDeleteState struct {
	remainingPolls   int
	lastErrorMessage *string
}

func New() *Server {
	s := &Server{
		Version:        "Qumulo Core 7.9.2.1",
		S3Enabled:      true,
		S3BasePath:     "/",
		RequireAuth:    true,
		Token:          "test-token",
		Buckets:        map[string]*bucketState{},
		Users:          map[string]*userState{},
		Quota:          map[string]int64{},
		Files:          map[string]string{},
		ACLs:           map[string]*qumulo.ACL{},
		Modes:          map[string]string{},
		treeDeleteJobs: map[string]*treeDeleteState{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/session/login", s.login)
	mux.HandleFunc("/v1/version", s.version)
	mux.HandleFunc("/v1/s3/settings", s.settings)
	mux.HandleFunc("/v1/s3/buckets/", s.buckets)
	mux.HandleFunc("/v1/s3/access-keys/", s.keys)
	mux.HandleFunc("/v1/users/", s.users)
	mux.HandleFunc("/v1/users", s.users)
	mux.HandleFunc("/v1/groups/", s.groups)
	mux.HandleFunc("/v1/groups", s.groups)
	mux.HandleFunc("/v1/files/quotas/", s.quotas)
	mux.HandleFunc("/v1/tree-delete/jobs/", s.treeDelete)
	mux.HandleFunc("/v1/files/", s.files)
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.wrap(mux)))
	return s
}

func (s *Server) wrap(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.RequireAuth && r.URL.Path != "/v1/session/login" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got != s.Token {
				writeErr(w, http.StatusUnauthorized, qumulo.ErrClassAuthInvalidCreds, "invalid token")
				return
			}
		}
		next.ServeHTTP(w, r)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Username == "" || in.Password == "" {
		writeErr(w, http.StatusUnauthorized, qumulo.ErrClassAuthInvalidCreds, "bad login")
		return
	}
	s.mu.Lock()
	s.Token = "sess-" + in.Username
	tok := s.Token
	s.mu.Unlock()
	writeJSON(w, 200, map[string]string{"bearer_token": tok})
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]string{"revision_id": s.Version})
}

func (s *Server) settings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, qumulo.S3Settings{Enabled: s.S3Enabled, BasePath: s.S3BasePath, HTTPS: true, Port: 9000})
}

func (s *Server) buckets(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/s3/buckets/")
	rest = strings.Trim(rest, "/")
	s.mu.Lock()
	defer s.mu.Unlock()

	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			if s.HideListN.Add(-1) >= 0 {
				writeJSON(w, 200, map[string]any{"buckets": []qumulo.Bucket{}})
				return
			}
			var list []qumulo.Bucket
			for _, b := range s.Buckets {
				if b == nil {
					continue
				}
				list = append(list, b.Bucket)
			}
			writeJSON(w, 200, map[string]any{"buckets": list})
		case http.MethodPost:
			var in qumulo.CreateBucketRequest
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				writeErr(w, 400, qumulo.ErrClassRESTInvalidRequest, err.Error())
				return
			}
			if in.Name == "" {
				writeErr(w, 400, qumulo.ErrClassRESTInvalidRequest, "name required")
				return
			}
			if s.RejectPrivate && in.Private != nil {
				writeErr(w, 400, qumulo.ErrClassRESTInvalidRequest, "unknown field 'private'")
				return
			}
			if _, ok := s.Buckets[in.Name]; ok {
				writeErr(w, 409, qumulo.ErrClassS3BucketExists, "bucket exists")
				return
			}
			path := in.Path
			if path == "" {
				path = "/" + in.Name
			}
			b := &bucketState{
				Bucket: qumulo.Bucket{
					Name:       in.Name,
					Path:       path,
					Versioning: "Unversioned",
				},
				Policy:  qumulo.EmptyPolicy(),
				ETag:    1,
				Uploads: map[string]qumulo.MultipartUpload{},
			}
			if in.ObjectLockEnabled != nil && *in.ObjectLockEnabled {
				b.LockConfig.Enabled = true
			}
			if in.Private != nil && *in.Private {
				b.Policy = qumulo.EmptyPolicy()
			}
			s.Buckets[in.Name] = b
			s.Files[path] = "fid-" + in.Name
			writeJSON(w, 200, b.Bucket)
		default:
			w.WriteHeader(405)
		}
		return
	}

	parts := strings.Split(rest, "/")
	name := parts[0]
	// Core 7.9.2.2 lock: there is no GET-by-name — it answers 405 whether
	// or not the bucket exists. Clients must list and filter. Modeling this
	// faithfully forces every unit test through the real fallback path.
	if len(parts) == 1 && r.Method == http.MethodGet {
		w.WriteHeader(405)
		return
	}
	b, ok := s.Buckets[name]
	if !ok {
		writeErr(w, 404, qumulo.ErrClassS3BucketNotFound, "no such bucket")
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodPatch:
			var in qumulo.PatchBucketRequest
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Versioning != nil {
				b.Versioning = *in.Versioning
			}
			if in.LockConfig != nil {
				b.LockConfig = *in.LockConfig
			}
			writeJSON(w, 200, b.Bucket)
		case http.MethodDelete:
			delRoot := r.URL.Query().Get("delete-root-dir") == "true"
			if delRoot && b.HasUploads {
				writeErr(w, 409, qumulo.ErrClassS3UploadsInProgress, "uploads in progress")
				return
			}
			if delRoot && b.NotEmpty {
				writeErr(w, 409, qumulo.ErrClassFSNotEmpty, "root dir not empty")
				return
			}
			delete(s.Buckets, name)
			w.WriteHeader(200)
		default:
			w.WriteHeader(405)
		}
		return
	}
	switch parts[1] {
	case "policy":
		s.policy(w, r, b)
	case "uploads":
		s.uploads(w, r, b, parts[2:])
	default:
		writeErr(w, 404, qumulo.ErrClassRESTNotFound, "unknown")
	}
}

func (s *Server) policy(w http.ResponseWriter, r *http.Request, b *bucketState) {
	switch r.Method {
	case http.MethodGet:
		// Live Core 7.9.2.2 returns the bare policy document with the ETag
		// in the header only — never a {policy, etag} wrapper. The wrapper
		// shape previously used here made the client's decodePolicy read
		// every policy as empty, masking lost statements in RMW tests.
		etag := strconv.Itoa(b.ETag)
		w.Header().Set("ETag", etag)
		writeJSON(w, 200, b.Policy)
	case http.MethodPut:
		if s.FailPolicyPut {
			writeErr(w, 400, qumulo.ErrClassRESTInvalidRequest, "injected policy PUT failure")
			return
		}
		if s.PolicyConflictN.Add(-1) >= 0 {
			writeErr(w, 412, qumulo.ErrClassRESTPrecondition, "etag mismatch")
			return
		}
		ifMatch := strings.Trim(r.Header.Get("If-Match"), `"`)
		if ifMatch != "" && ifMatch != strconv.Itoa(b.ETag) {
			writeErr(w, 412, qumulo.ErrClassRESTPrecondition, "etag mismatch")
			return
		}
		var p qumulo.Policy
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeErr(w, 400, qumulo.ErrClassRESTInvalidRequest, err.Error())
			return
		}
		b.Policy = &p
		b.ETag++
		w.WriteHeader(200)
	default:
		w.WriteHeader(405)
	}
}

func (s *Server) uploads(w http.ResponseWriter, r *http.Request, b *bucketState, rest []string) {
	if len(rest) == 0 {
		var list []qumulo.MultipartUpload
		for _, u := range b.Uploads {
			list = append(list, u)
		}
		writeJSON(w, 200, map[string]any{"uploads": list})
		return
	}
	id := rest[0]
	if r.Method == http.MethodDelete {
		delete(b.Uploads, id)
		if len(b.Uploads) == 0 {
			b.HasUploads = false
		}
		w.WriteHeader(200)
		return
	}
	w.WriteHeader(405)
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/users"), "/")
	switch {
	case id == "" && r.Method == http.MethodGet:
		var list []qumulo.LocalUser
		for _, u := range s.Users {
			list = append(list, u.LocalUser)
		}
		writeJSON(w, 200, map[string]any{"users": list})
	case id == "" && r.Method == http.MethodPost:
		var in struct {
			Name         string `json:"name"`
			Password     string `json:"password"`
			PrimaryGroup string `json:"primary_group"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if _, ok := s.Users[in.Name]; ok {
			writeErr(w, 409, qumulo.ErrClassAuthUserExists, "user exists")
			return
		}
		u := &userState{LocalUser: qumulo.LocalUser{ID: "uid-" + strconv.FormatInt(s.UserSeq.Add(1), 10) + "-" + in.Name, Name: in.Name, PrimaryGroup: in.PrimaryGroup}, Password: in.Password}
		s.Users[in.Name] = u
		writeJSON(w, 200, u.LocalUser)
	case id != "" && r.Method == http.MethodDelete:
		for name, u := range s.Users {
			if u.ID == id || name == id {
				delete(s.Users, name)
				w.WriteHeader(200)
				return
			}
		}
		writeErr(w, 404, qumulo.ErrClassAuthNoSuchUser, "no user")
	default:
		w.WriteHeader(405)
	}
}

func (s *Server) keys(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/s3/access-keys"), "/")
	switch {
	case id == "" && r.Method == http.MethodGet:
		user := r.URL.Query().Get("user")
		var entries []qumulo.AccessKey
		for _, u := range s.Users {
			// ?user= accepts an auth_id or a name.
			if user != "" && u.Name != user && u.ID != user {
				continue
			}
			for _, k := range u.Keys {
				pub := k.AccessKey
				pub.SecretAccessKey = "" // never retrievable again
				entries = append(entries, pub)
			}
		}
		writeJSON(w, 200, map[string]any{"entries": entries})
	case id == "" && r.Method == http.MethodPost:
		var in struct {
			User qumulo.Identity `json:"user"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		var u *userState
		for _, cand := range s.Users {
			if (in.User.AuthID != "" && cand.ID == in.User.AuthID) || (in.User.Name != "" && cand.Name == in.User.Name) {
				u = cand
				break
			}
		}
		if u == nil {
			writeErr(w, 404, qumulo.ErrClassAuthNoSuchUser, "no user")
			return
		}
		if len(u.Keys) >= 2 {
			writeErr(w, 409, qumulo.ErrClassS3KeyLimit, "2 key limit")
			return
		}
		kid := "AKIA" + randHex(8)
		sec := "secret-" + randHex(16)
		k := storedKey{AccessKey: qumulo.AccessKey{
			AccessKeyID:     kid,
			SecretAccessKey: sec,
			Owner:           qumulo.Identity{Name: u.Name, AuthID: u.ID, Domain: "LOCAL"},
		}}
		u.Keys = append(u.Keys, k)
		writeJSON(w, 200, k.AccessKey)
	case id != "" && r.Method == http.MethodDelete:
		for _, u := range s.Users {
			for i, k := range u.Keys {
				if k.AccessKeyID == id {
					u.Keys = append(u.Keys[:i], u.Keys[i+1:]...)
					w.WriteHeader(200)
					return
				}
			}
		}
		writeErr(w, 404, qumulo.ErrClassRESTNotFound, "no key")
	default:
		w.WriteHeader(405)
	}
}

func (s *Server) groups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	writeJSON(w, 200, []qumulo.LocalGroup{
		{ID: "513", Name: "Users"},
		{ID: "514", Name: "Guests"},
	})
}

func parseFileRef(r *http.Request) (path, suffix string) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/files/")
	for _, suf := range []string{"/info/attributes", "/info/acl", "/entries/", "/entries"} {
		if i := strings.Index(rest, suf); i >= 0 {
			raw := rest[:i]
			if dec, err := url.PathUnescape(raw); err == nil {
				raw = dec
			}
			return "/" + strings.Trim(raw, "/"), suf
		}
	}
	return "/" + strings.Trim(rest, "/"), ""
}

func (s *Server) lookupFile(p string) (string, bool) {
	if id, ok := s.Files[p]; ok {
		return id, true
	}
	for path, fid := range s.Files {
		if strings.Trim(path, "/") == strings.Trim(p, "/") {
			return fid, true
		}
	}
	return "", false
}

func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	p, suffix := parseFileRef(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.lookupFile(p)
	if !ok {
		writeErr(w, 404, qumulo.ErrClassFSNoSuchEntry, "no file")
		return
	}
	switch suffix {
	case "/info/attributes":
		switch r.Method {
		case http.MethodGet:
			mode := s.Modes[p]
			if mode == "" {
				mode = "0755"
			}
			writeJSON(w, 200, qumulo.FileAttributes{ID: id, Path: p, Mode: mode})
		case http.MethodPatch:
			var in struct {
				Mode string `json:"mode"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Mode != "" {
				s.Modes[p] = in.Mode
			}
			writeJSON(w, 200, qumulo.FileAttributes{ID: id, Path: p, Mode: s.Modes[p]})
		default:
			w.WriteHeader(405)
		}
	case "/info/acl":
		switch r.Method {
		case http.MethodGet:
			if s.FailACLGet {
				writeErr(w, 500, "internal_error", "injected ACL GET failure")
				return
			}
			acl := s.ACLs[p]
			if acl == nil {
				acl = &qumulo.ACL{Control: []string{"PRESENT"}, ACES: []qumulo.ACE{}}
			}
			writeJSON(w, 200, map[string]any{"generated": false, "acl": acl})
		case http.MethodPut:
			if s.FailACLPut {
				writeErr(w, 400, qumulo.ErrClassRESTInvalidRequest, "injected ACL PUT failure")
				return
			}
			// Core 7.9.2.2 lock: every ACE must carry a "flags" field —
			// omitting it fails the whole aces array with decode_error.
			var raw struct {
				ACES []map[string]json.RawMessage `json:"aces"`
			}
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &raw); err != nil {
				writeErr(w, 400, qumulo.ErrClassRESTInvalidRequest, err.Error())
				return
			}
			for _, ace := range raw.ACES {
				if _, ok := ace["flags"]; !ok {
					writeErr(w, 400, "decode_error", "value for field 'aces' could not be parsed")
					return
				}
			}
			var acl qumulo.ACL
			if err := json.Unmarshal(body, &acl); err != nil {
				writeErr(w, 400, qumulo.ErrClassRESTInvalidRequest, err.Error())
				return
			}
			cp := acl
			s.ACLs[p] = &cp
			w.WriteHeader(200)
		default:
			w.WriteHeader(405)
		}
	default:
		writeJSON(w, 200, qumulo.FileAttributes{ID: id, Path: p})
	}
}

func (s *Server) quotas(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/files/quotas"), "/")
	switch r.Method {
	case http.MethodGet:
		limit, ok := s.Quota[id]
		if !ok {
			writeErr(w, http.StatusNotFound, qumulo.ErrClassRESTNotFound, "quota not found")
			return
		}
		writeJSON(w, http.StatusOK, qumulo.Quota{ID: id, Limit: limit})
	case http.MethodPost:
		var q qumulo.Quota
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil || q.ID == "" {
			writeErr(w, http.StatusBadRequest, qumulo.ErrClassRESTInvalidRequest, "invalid quota")
			return
		}
		if _, exists := s.Quota[q.ID]; exists {
			writeErr(w, http.StatusConflict, qumulo.ErrClassQuotaExists, "quota exists")
			return
		}
		s.Quota[q.ID] = q.Limit
		writeJSON(w, http.StatusOK, q)
	case http.MethodPut:
		if _, exists := s.Quota[id]; !exists {
			writeErr(w, http.StatusNotFound, qumulo.ErrClassRESTNotFound, "quota not found")
			return
		}
		var q qumulo.Quota
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			writeErr(w, http.StatusBadRequest, qumulo.ErrClassRESTInvalidRequest, "invalid quota")
			return
		}
		q.ID = id
		s.Quota[id] = q.Limit
		writeJSON(w, http.StatusOK, q)
	case http.MethodDelete:
		delete(s.Quota, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) treeDelete(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/tree-delete/jobs/")
	switch {
	case r.Method == http.MethodPost && rest == "":
		s.createTreeDelete(w, r)
	case r.Method == http.MethodGet && rest != "":
		id, err := url.PathUnescape(rest)
		if err != nil || id == "" {
			writeErr(w, http.StatusBadRequest, qumulo.ErrClassRESTInvalidRequest, "invalid tree-delete job ID")
			return
		}
		s.getTreeDelete(w, id)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) createTreeDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.ID) == "" {
		writeErr(w, http.StatusBadRequest, qumulo.ErrClassRESTInvalidRequest, "invalid tree-delete job")
		return
	}
	if s.FailTreeDeleteN.Add(-1) >= 0 {
		writeErr(w, http.StatusServiceUnavailable, "internal_error", "injected tree-delete failure")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.treeDeleteJobs[in.ID]; exists {
		writeErr(w, http.StatusConflict, qumulo.ErrClassFSEntryExists, "tree-delete job already exists")
		return
	}
	targetExists := false
	for _, fid := range s.Files {
		if fid == in.ID {
			targetExists = true
			break
		}
	}
	if !targetExists {
		writeErr(w, http.StatusNotFound, qumulo.ErrClassFSNoSuchEntry, "tree-delete target not found")
		return
	}

	state := &treeDeleteState{remainingPolls: s.TreeDeletePolls}
	if s.TreeDeleteError != "" {
		message := s.TreeDeleteError
		state.lastErrorMessage = &message
	}
	s.TreeJobs = append(s.TreeJobs, in.ID)
	s.treeDeleteJobs[in.ID] = state
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) getTreeDelete(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, exists := s.treeDeleteJobs[id]
	if !exists {
		writeErr(w, http.StatusNotFound, qumulo.ErrClassRESTNotFound, "tree-delete job not found")
		return
	}
	if state.lastErrorMessage != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":                 id,
			"status":             "ABORTED",
			"aborted":            true,
			"last_error_message": *state.lastErrorMessage,
		})
		return
	}
	if state.remainingPolls > 0 {
		state.remainingPolls--
		writeJSON(w, http.StatusOK, map[string]any{
			"id":                 id,
			"status":             "RUNNING",
			"last_error_message": nil,
		})
		return
	}

	delete(s.treeDeleteJobs, id)
	for path, fid := range s.Files {
		if fid == id {
			delete(s.Files, path)
		}
	}
	writeErr(w, http.StatusNotFound, qumulo.ErrClassRESTNotFound, "tree-delete job finished")
}

func (s *Server) SeedUpload(bucket, uploadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.Buckets[bucket]; ok {
		b.Uploads[uploadID] = qumulo.MultipartUpload{UploadID: uploadID, Key: "part"}
		b.HasUploads = true
	}
}

func (s *Server) MarkNotEmpty(bucket string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.Buckets[bucket]; ok {
		b.NotEmpty = true
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, class, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(qumulo.APIError{
		ErrorClass:  class,
		Description: desc,
		UserVisible: true,
	})
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

var _ = io.EOF
