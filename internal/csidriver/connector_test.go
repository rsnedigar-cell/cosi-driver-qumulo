package csidriver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControllerAuthDoesNotMixRequestCAWithConfiguredCredential(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("mounted-admin-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("configured-ca"), 0o600); err != nil {
		t.Fatal(err)
	}
	connector := newQumuloConnector(Config{CredentialsDir: dir}, nil)
	if _, _, err := connector.controllerAuth(map[string]string{"ca.crt": "attacker-ca"}); err == nil {
		t.Fatal("request CA without request credentials selected configured admin credentials")
	}

	creds, ca, err := connector.controllerAuth(nil)
	if err != nil {
		t.Fatal(err)
	}
	if creds.Token != "mounted-admin-token" || string(ca) != "configured-ca" {
		t.Fatalf("unexpected configured authentication source: creds=%#v ca=%q", creds, ca)
	}
}

func TestControllerAuthKeepsRequestCredentialsAndCATogether(t *testing.T) {
	connector := newQumuloConnector(Config{}, nil)
	creds, ca, err := connector.controllerAuth(map[string]string{"qumuloToken": "request-token", "ca.pem": "request-ca"})
	if err != nil {
		t.Fatal(err)
	}
	if creds.Token != "request-token" || string(ca) != "request-ca" {
		t.Fatalf("unexpected request authentication source: creds=%#v ca=%q", creds, ca)
	}
	if _, _, err := connector.controllerAuth(map[string]string{"qumuloToken": "request-token", "qumuloUsername": "alice"}); err == nil {
		t.Fatal("mixed token and password credential fields were accepted")
	}
	if _, _, err := connector.controllerAuth(map[string]string{"qumuloUsername": "alice"}); err == nil {
		t.Fatal("incomplete request credential was accepted")
	}
	if _, _, err := connector.controllerAuth(map[string]string{"qumuloToken": "request-token", "qumuloUsername": ""}); err == nil {
		t.Fatal("present-but-empty username was ignored alongside a request token")
	}
	if _, _, err := connector.controllerAuth(map[string]string{"qumuloToken": "request-token", "ca.crt": ""}); err == nil {
		t.Fatal("present-but-empty request CA was silently replaced with configured trust")
	}
}

func TestControllerAuthMountedCredentialSourceIsAtomic(t *testing.T) {
	t.Setenv("QUMULO_TOKEN", "environment-token")
	t.Setenv("QUMULO_USERNAME", "")
	t.Setenv("QUMULO_PASSWORD", "")

	tests := []struct {
		name  string
		files map[string]string
		dirs  []string
		want  string
	}{
		{name: "empty token", files: map[string]string{"token": " \n"}, want: "present but empty"},
		{name: "partial password pair", files: map[string]string{"username": "alice"}, want: "both be present"},
		{name: "mixed authentication", files: map[string]string{"token": "mounted-token", "username": "alice", "password": "secret"}, want: "cannot combine"},
		{name: "unreadable token path", dirs: []string{"token"}, want: "read mounted credential"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, value := range test.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range test.dirs {
				if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			connector := newQumuloConnector(Config{CredentialsDir: dir}, nil)
			creds, _, err := connector.controllerAuth(nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("controllerAuth() creds=%#v err=%v, want error containing %q", creds, err, test.want)
			}
			if creds.Token == "environment-token" {
				t.Fatal("invalid mounted credential material fell through to environment credentials")
			}
		})
	}
}

func TestControllerAuthEnvironmentCredentialShape(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		username string
		password string
		want     string
	}{
		{name: "partial pair", username: "alice", want: "both be set"},
		{name: "token and password pair", token: "token", username: "alice", password: "secret", want: "cannot combine"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("QUMULO_TOKEN", test.token)
			t.Setenv("QUMULO_USERNAME", test.username)
			t.Setenv("QUMULO_PASSWORD", test.password)
			connector := newQumuloConnector(Config{}, nil)
			if _, _, err := connector.controllerAuth(nil); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("controllerAuth() error=%v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestControllerAuthRejectsEmptyConfiguredCA(t *testing.T) {
	t.Setenv("QUMULO_TOKEN", "environment-token")
	t.Setenv("QUMULO_USERNAME", "")
	t.Setenv("QUMULO_PASSWORD", "")

	t.Run("explicit file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.crt")
		if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		connector := newQumuloConnector(Config{CAFile: path}, nil)
		if _, _, err := connector.controllerAuth(nil); err == nil || !strings.Contains(err.Error(), "file is empty") {
			t.Fatalf("controllerAuth() error=%v, want empty explicit CA error", err)
		}
	})

	t.Run("mounted file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte(" \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		connector := newQumuloConnector(Config{CredentialsDir: dir}, nil)
		if _, _, err := connector.controllerAuth(nil); err == nil || !strings.Contains(err.Error(), "file is empty") {
			t.Fatalf("controllerAuth() error=%v, want empty mounted CA error", err)
		}
	})
}

func TestControllerAuthCarriesMountedCAToEnvironmentCredential(t *testing.T) {
	t.Setenv("QUMULO_TOKEN", "environment-token")
	t.Setenv("QUMULO_USERNAME", "")
	t.Setenv("QUMULO_PASSWORD", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("mounted-ca"), 0o600); err != nil {
		t.Fatal(err)
	}

	connector := newQumuloConnector(Config{CredentialsDir: dir}, nil)
	creds, ca, err := connector.controllerAuth(nil)
	if err != nil {
		t.Fatal(err)
	}
	if creds.Token != "environment-token" || string(ca) != "mounted-ca" {
		t.Fatalf("unexpected environment authentication source: creds=%#v ca=%q", creds, ca)
	}
}

func TestControllerAuthRequestCADoesNotReadUnusedConfiguredCA(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty-ca.crt")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	connector := newQumuloConnector(Config{CAFile: path}, nil)
	creds, ca, err := connector.controllerAuth(map[string]string{"qumuloToken": "request-token", "ca.crt": "request-ca"})
	if err != nil {
		t.Fatal(err)
	}
	if creds.Token != "request-token" || string(ca) != "request-ca" {
		t.Fatalf("unexpected request authentication source: creds=%#v ca=%q", creds, ca)
	}
}
