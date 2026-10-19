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

func TestPostgresDSNRedactedKeepsPasswordFreeDSN(t *testing.T) {
	const dsn = "postgres://u@127.0.0.1:5432/db?sslmode=disable"
	if got := (PostgresDSN{raw: dsn}).Redacted(); got != dsn {
		t.Errorf("Redacted() = %q, want %q unchanged", got, dsn)
	}
}

func TestPostgresDSNFormattingNeverPrintsRaw(t *testing.T) {
	dsn := PostgresDSN{raw: "postgres://u:FMTSECRET@host/db"}
	encoded, err := json.Marshal(PostgresSettings{DSN: dsn})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, out := range []string{fmt.Sprint(dsn), fmt.Sprintf("%v %+v %#v", dsn, dsn, dsn), string(encoded)} {
		if strings.Contains(out, "FMTSECRET") {
			t.Errorf("formatted DSN %q contains the secret", out)
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
