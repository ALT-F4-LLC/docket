package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestPostgresDSNRedactedHidesEverySecret(t *testing.T) {
	cases := []struct {
		name, dsn, secret string
	}{
		{"userinfo password", "postgres://u:USERSECRET@host/db", "USERSECRET"},
		{"password query parameter", "postgres://u@host/db?password=QUERYSECRET", "QUERYSECRET"},
		{"upper-case key", "postgres://u@host/db?PASSWORD=CASESECRET", "CASESECRET"},
		{"percent-encoded key", "postgres://u@host/db?pass%77ord=ENCSECRET", "ENCSECRET"},
		{"sslpassword", "postgresql://u@host/db?sslpassword=SSLSECRET", "SSLSECRET"},
		{"duplicate key", "postgres://u@host/db?sslmode=disable&password=a&password=DUPSECRET", "DUPSECRET"},
		{"key/value form", "host=h user=u password=KVSECRET dbname=d", "KVSECRET"},
		{"malformed escape", "postgres://u:BAD%zzSECRET@host/db", "SECRET"},
		{"malformed query", "postgres://u@host/db?password=QSECRET%zz", "QSECRET"},
		{"fragment", "postgres://u:p@host/db#FRAGSECRET", "FRAGSECRET"},
		{"opaque", "postgres:OPAQUESECRET", "OPAQUESECRET"},
		{"secret query key", "postgres://u@host/db?KEYSECRET=x", "KEYSECRET"},
		{"secret query key without value", "postgres://u@host/db?KEYSECRET", "KEYSECRET"},
		{"password tail after unescaped ampersand", "postgres://u@host/db?password=ab&AMPSECRET", "AMPSECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := PostgresDSN{raw: tc.dsn}
			got := dsn.Redacted()
			if strings.Contains(got, tc.secret) {
				t.Errorf("Redacted() = %q, which contains the secret %q", got, tc.secret)
			}
			if !strings.Contains(got, RedactedMarker) {
				t.Errorf("Redacted() = %q, want the marker %q", got, RedactedMarker)
			}
		})
	}
}

func TestPostgresDSNRedactedHidesUsername(t *testing.T) {
	cases := []struct {
		name, dsn string
		keepsHost bool
	}{
		{"userinfo username and password", "postgres://USERNAMESECRET:PWSECRET@host/db", true},
		{"userinfo username only", "postgres://USERNAMESECRET@host/db", true},
		{"percent-encoded userinfo username", "postgres://%55SERNAMESECRET@host/db", true},
		{"user query parameter", "postgres://host/db?user=USERNAMESECRET", true},
		{"percent-encoded user key", "postgres://host/db?us%65r=USERNAMESECRET", true},
		{"duplicate user key", "postgres://host/db?user=a&user=USERNAMESECRET", true},
		{"key/value form", "host=h user=USERNAMESECRET password=PWSECRET dbname=d", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := (PostgresDSN{raw: tc.dsn}).Redacted()
			for _, secret := range []string{"USERNAMESECRET", "PWSECRET"} {
				if strings.Contains(got, secret) {
					t.Errorf("Redacted() = %q, which contains the secret %q", got, secret)
				}
			}
			if !strings.Contains(got, RedactedMarker) {
				t.Errorf("Redacted() = %q, want the marker %q", got, RedactedMarker)
			}
			if tc.keepsHost && !strings.Contains(got, "host/db") {
				t.Errorf("Redacted() = %q, want the host and database kept", got)
			}
		})
	}
}

func TestPostgresDSNRedactedCollapsesMisparsedUserinfo(t *testing.T) {
	cases := []struct {
		name, dsn string
	}{
		{"unescaped slash after digit prefix", "postgres://u:12/PATHSECRET@host/db"},
		{"password holding at-sign and slash", "postgres://u:p@s/TAILSECRET@host/db"},
		{"unescaped hash", "postgres://u:12#HASHSECRET@host/db"},
		{"unescaped question mark before displayable key", "postgres://u:12?sslmode=QSECRET@host/db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (PostgresDSN{raw: tc.dsn}).Redacted(); got != RedactedMarker {
				t.Errorf("Redacted() = %q, want exactly %q", got, RedactedMarker)
			}
		})
	}
}

func TestPostgresDSNRedactedKeepsHostForUsernameOnlyDSN(t *testing.T) {
	if got := (PostgresDSN{raw: "postgres://u@host/db"}).Redacted(); !strings.Contains(got, "host/db") {
		t.Errorf("Redacted() = %q, want the host and database kept", got)
	}
}

func TestPostgresDSNRedactedKeepsPasswordFreeDSN(t *testing.T) {
	const dsn = "postgres://u@127.0.0.1:5432/db?application_name=x&sslmode=disable"
	const want = "postgres://" + RedactedMarker + "@127.0.0.1:5432/db?application_name=x&sslmode=disable"
	if got := (PostgresDSN{raw: dsn}).Redacted(); got != want {
		t.Errorf("Redacted() = %q, want %q", got, want)
	}
}

func TestPostgresDSNFormattingNeverPrintsRaw(t *testing.T) {
	dsn := PostgresDSN{raw: "postgres://FMTUSERSECRET:FMTSECRET@host/db"}
	encoded, err := json.Marshal(PostgresSettings{DSN: dsn})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, out := range []string{fmt.Sprint(dsn), fmt.Sprintf("%v %+v %#v", dsn, dsn, dsn), string(encoded)} {
		for _, secret := range []string{"FMTUSERSECRET", "FMTSECRET"} {
			if strings.Contains(out, secret) {
				t.Errorf("formatted DSN %q contains the secret %q", out, secret)
			}
		}
	}
}

func TestPostgresFromEnv(t *testing.T) {
	t.Setenv(PostgresDSNEnv, "postgres://u@host/db")
	t.Setenv(PostgresPasswordFileEnv, "/run/secrets/pg")
	got := PostgresFromEnv()
	if got.DSN.Raw() != "postgres://u@host/db" || got.PasswordFile != "/run/secrets/pg" {
		t.Errorf("PostgresFromEnv() = {%q, %q}", got.DSN.Raw(), got.PasswordFile)
	}

	t.Setenv(PostgresDSNEnv, "")
	if got := PostgresFromEnv(); got.DSN.IsSet() || got.DSN.Redacted() != "" {
		t.Errorf("unset DSN: IsSet() = %v, Redacted() = %q", got.DSN.IsSet(), got.DSN.Redacted())
	}
}
