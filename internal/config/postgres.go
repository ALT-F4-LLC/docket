package config

import (
	"net/url"
	"os"
)

// RedactedMarker replaces every secret a displayed DSN carries. It needs no
// URL escaping, so it reads the same in the userinfo and the query.
const RedactedMarker = "REDACTED"

// Environment variables naming the PostgreSQL settings. They come only from
// the user's environment, never from repository content, because the DSN
// decides where the password file's contents are sent.
const (
	PostgresDSNEnv          = "DOCKET_POSTGRES_DSN"
	PostgresPasswordFileEnv = "DOCKET_POSTGRES_PASSWORD_FILE"
)

// displayableDSNKeys are the query keys whose values Redacted prints as is:
// the keys pgstore allows a DSN to set, except "user", because the username
// is secret in displayed output. Any other key is treated as secret.
var displayableDSNKeys = map[string]bool{
	"host": true, "port": true, "dbname": true, "sslmode": true,
	"sslrootcert": true, "sslcert": true, "sslkey": true,
	"connect_timeout": true, "application_name": true,
}

// PostgresSettings is the PostgreSQL connection configuration.
type PostgresSettings struct {
	DSN PostgresDSN
	// PasswordFile is the path of the file holding the role password. Nothing
	// in this package opens it.
	PasswordFile string
}

// PostgresFromEnv reads the PostgreSQL settings from the environment. An
// unset variable leaves its setting empty.
func PostgresFromEnv() PostgresSettings {
	return PostgresSettings{
		DSN:          PostgresDSN{raw: os.Getenv(PostgresDSNEnv)},
		PasswordFile: os.Getenv(PostgresPasswordFileEnv),
	}
}

// PostgresDSN holds a connection string that may carry a password by
// misconfiguration. Formatting it prints the redacted form, and its only
// field is unexported, so neither fmt nor encoding/json prints the raw value.
type PostgresDSN struct {
	raw string
}

// IsSet reports whether a DSN was configured.
func (d PostgresDSN) IsSet() bool { return d.raw != "" }

// Raw returns the DSN as configured, for connecting. Never display it.
func (d PostgresDSN) Raw() string { return d.raw }

func (d PostgresDSN) String() string   { return d.Redacted() }
func (d PostgresDSN) GoString() string { return "config.PostgresDSN{" + d.Redacted() + "}" }

// Redacted renders the DSN for display. It keeps the scheme, host, port,
// database path, and the values of displayableDSNKeys, and replaces the
// userinfo username and password and every other query value with
// RedactedMarker. It
// drops any fragment. A DSN that is not a postgres:// or postgresql:// URL,
// including the key/value form, or that fails to parse, renders as the marker
// alone: neither the input nor a parse error that quotes it is returned. An
// unset DSN renders as "".
func (d PostgresDSN) Redacted() string {
	if d.raw == "" {
		return ""
	}
	u, err := url.Parse(d.raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Opaque != "" {
		return RedactedMarker
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return RedactedMarker
	}
	for key, values := range query {
		if displayableDSNKeys[key] {
			continue
		}
		for i := range values {
			values[i] = RedactedMarker
		}
	}

	shown := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawQuery: query.Encode()}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			shown.User = url.UserPassword(RedactedMarker, RedactedMarker)
		} else {
			shown.User = url.User(RedactedMarker)
		}
	}
	return shown.String()
}
