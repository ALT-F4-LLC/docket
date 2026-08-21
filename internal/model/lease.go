package model

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"time"
)

// TokenBytes is the entropy of a capability token, in bytes. 256 bits, hex
// encoded to a 64-character string.
const TokenBytes = 32

// Lease is a capability-mediated hold on an entity (engine-core.md §5:
// "Claims are capabilities"). A claim mints a token and takes a lease; the
// holder renews it by heartbeating; expiry without release returns the entity
// to the unclaimed pool and is the liveness mechanism.
//
// The fields mirror the v6 columns exactly, and are shaped for the steps table
// to reuse verbatim (engine-spec.md §10 stage 2). Nothing here is
// issue-specific — a lease is a lease.
type Lease struct {
	// Owner identifies the holder. Empty means unclaimed.
	Owner string
	// TokenHash is the SHA-256 hex of the capability token. The token itself
	// is returned exactly once, at claim, and is never stored.
	TokenHash string
	// ExpiresMS is the lease expiry in epoch milliseconds.
	ExpiresMS int64
	// Attempt counts claims made against the entity, ever. It is monotonic:
	// never decremented, never reset, so the trail of a died-mid-work holder
	// survives its replacement (engine-spec.md §9 item 4).
	Attempt int
}

// Held reports whether a lease exists at all, live or lapsed.
func (l *Lease) Held() bool { return l != nil && l.Owner != "" }

// Live reports whether the lease is held AND unexpired at nowMS.
//
// This is computed per read and never written back: engine-spec.md §2 requires
// read verbs to render *effective* status with lease expiry computed at read
// time, and §6 confines every lease write to claim. An expired lease therefore
// reads as dead the instant it lapses, even though the row still carries the
// stale owner until someone claims it — status never lies just because nobody
// called claim.
func (l *Lease) Live(nowMS int64) bool {
	return l.Held() && l.ExpiresMS > nowMS
}

// leaseJSON is the v2 wire shape for a lease. It is emitted only under
// --json=v2 and only when a lease is held, so v1 output and unclaimed-entity
// output stay byte-identical to v5 (engine-spec.md §9 item 8).
type leaseJSON struct {
	Owner     string `json:"owner"`
	ExpiresMS int64  `json:"expires_ms"`
	Attempt   int    `json:"attempt"`
	Live      bool   `json:"live"`
}

// LeaseWire renders the lease for --json=v2 at nowMS, or nil when no lease is
// held. Callers marshal the result directly; a nil result must be omitted from
// the payload rather than emitted as null.
func (l *Lease) LeaseWire(nowMS int64) any {
	if !l.Held() {
		return nil
	}
	return leaseJSON{
		Owner:     l.Owner,
		ExpiresMS: l.ExpiresMS,
		Attempt:   l.Attempt,
		Live:      l.Live(nowMS),
	}
}

// MintToken generates a capability token and returns it with its hash.
//
// crypto/rand specifically: engine-core.md §5 requires a token "not derivable
// from ids", and a math/rand token is derivable from the seed. A read error is
// a hard failure with no fallback path — a fallback is exactly how a weak
// token reaches production.
func MintToken() (token, hash string, err error) {
	buf := make([]byte, TokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("minting capability token: %w", err)
	}
	token = hex.EncodeToString(buf)
	return token, HashToken(token), nil
}

// HashToken returns the SHA-256 hex of a capability token.
//
// A plain SHA-256 is correct here and a password KDF is not: the token is 256
// bits of uniform entropy, so there is no dictionary to attack and no work
// factor to raise. Hashing exists so a stolen database file yields no live
// capability, which this achieves completely.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// TokenMatches reports whether a presented token matches a stored hash.
//
// Constant-time: a timing oracle on a 64-hex comparison is a real, if slow,
// byte-at-a-time recovery path, and the correct primitive costs one import.
func TokenMatches(token, hash string) bool {
	if token == "" || hash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(HashToken(token)), []byte(hash)) == 1
}

// NowMS returns the current time in epoch milliseconds — the unit every v6
// column and wire field uses.
func NowMS() int64 { return time.Now().UTC().UnixMilli() }
