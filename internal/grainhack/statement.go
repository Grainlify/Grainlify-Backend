// Package grainhack issues GrainHack results statements and records what the
// bounty agent reports back about paying them.
//
// The contract this implements is drafts/grainhack-payout-contract.md, section 1
// (the statement) and the backend half of section 3 (payment reports and
// notifications). The agent and the grainhack-signer verify what this package
// signs, byte for byte, so the canonical form below is an interface: change it
// in all three places or none. testdata/grainhack_results_golden.json pins it.
package grainhack

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// SignatureDomain prefixes every message the results key signs, so a signature
// from it can never be read as anything but a GrainHack results statement.
const SignatureDomain = "grainlify-grainhack-results:v1\n"

// Statement constants, as the contract spells them.
const (
	StatementVersion = 1
	StatementKind    = "grainhack_results"
	CurrencyUSDC     = "USDC"

	PoolContributor = "contributor"
	PoolMaintainer  = "maintainer"

	NetworkSolanaDevnet  = "solana-devnet"
	NetworkSolanaMainnet = "solana-mainnet"

	StatusPayable = "payable"
	StatusHeldKYC = "held_kyc"
)

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER. github_user_id is a
// JSON number in the statement and the agent reads it in JavaScript; above this
// it would silently round to a different person's id.
const maxSafeInteger = 1<<53 - 1

// ErrInvalidStatement means a statement could not be put in canonical form
// because it breaks one of the contract's rules. Nothing is signed.
var ErrInvalidStatement = errors.New("grainhack: invalid results statement")

// Line is one winner's share of the pool.
type Line struct {
	GitHubUserID int64
	Login        string
	AmountMinor  *big.Int
	Status       string
}

// Statement is a results statement before it is canonicalised and signed.
type Statement struct {
	StatementID   uuid.UUID
	Supersedes    *uuid.UUID
	HackathonID   uuid.UUID
	HackathonName string
	Pool          string
	ComputationID uuid.UUID
	Currency      string
	Network       string
	PoolMinor     *big.Int
	Lines         []Line
	IssuedAt      time.Time
}

// ValidNetwork reports whether n is a network a statement may name.
func ValidNetwork(n string) bool {
	return n == NetworkSolanaDevnet || n == NetworkSolanaMainnet
}

// FormatIssuedAt is the one way issued_at is written: RFC3339, UTC, whole
// seconds, "Z". Sub-second precision is dropped rather than written, so the
// string is the same whichever clock produced it.
func FormatIssuedAt(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
}

// Canonical returns the statement's canonical JSON: keys sorted at every level,
// no whitespace, money as decimal strings, lines sorted by github_user_id
// ascending. It is what JavaScript's JSON.stringify produces for the same
// object with its keys sorted, including how strings are escaped.
//
// It refuses (ErrInvalidStatement) rather than emitting anything the contract
// does not allow - in particular, lines that do not sum to pool_minor.
func (s Statement) Canonical() (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}

	lines := make([]Line, len(s.Lines))
	copy(lines, s.Lines)
	sort.Slice(lines, func(i, j int) bool { return lines[i].GitHubUserID < lines[j].GitHubUserID })

	lineObjs := make([]any, 0, len(lines))
	for _, l := range lines {
		lineObjs = append(lineObjs, map[string]any{
			"amount_minor":   l.AmountMinor.String(),
			"github_user_id": l.GitHubUserID,
			"login":          l.Login,
			"status":         l.Status,
		})
	}

	var supersedes any // JSON null
	if s.Supersedes != nil {
		supersedes = s.Supersedes.String()
	}

	obj := map[string]any{
		"v":              int64(StatementVersion),
		"kind":           StatementKind,
		"statement_id":   s.StatementID.String(),
		"supersedes":     supersedes,
		"hackathon_id":   s.HackathonID.String(),
		"hackathon_name": s.HackathonName,
		"pool":           s.Pool,
		"computation_id": s.ComputationID.String(),
		"currency":       s.Currency,
		"network":        s.Network,
		"pool_minor":     s.PoolMinor.String(),
		"lines":          lineObjs,
		"issued_at":      FormatIssuedAt(s.IssuedAt),
	}
	var b strings.Builder
	if err := writeCanonical(&b, obj); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (s Statement) validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidStatement, fmt.Sprintf(format, args...))
	}
	if s.StatementID == uuid.Nil || s.HackathonID == uuid.Nil || s.ComputationID == uuid.Nil {
		return bad("statement, hackathon and computation ids are all required")
	}
	if s.Supersedes != nil && (*s.Supersedes == uuid.Nil || *s.Supersedes == s.StatementID) {
		return bad("supersedes must name a different statement")
	}
	if s.Pool != PoolContributor && s.Pool != PoolMaintainer {
		return bad("unknown pool %q", s.Pool)
	}
	if s.Currency != CurrencyUSDC {
		return bad("currency %q is not USDC", s.Currency)
	}
	if !ValidNetwork(s.Network) {
		return bad("unknown network %q", s.Network)
	}
	if s.IssuedAt.IsZero() {
		return bad("issued_at is required")
	}
	if s.PoolMinor == nil || s.PoolMinor.Sign() <= 0 {
		return bad("pool_minor must be positive")
	}
	if len(s.Lines) == 0 {
		return bad("a statement has at least one line")
	}
	sum := new(big.Int)
	seen := map[int64]bool{}
	for _, l := range s.Lines {
		if l.GitHubUserID <= 0 || l.GitHubUserID > maxSafeInteger {
			return bad("github_user_id %d is out of range", l.GitHubUserID)
		}
		if seen[l.GitHubUserID] {
			return bad("github_user_id %d appears twice", l.GitHubUserID)
		}
		seen[l.GitHubUserID] = true
		if l.Login == "" {
			return bad("github_user_id %d has no login", l.GitHubUserID)
		}
		if l.AmountMinor == nil || l.AmountMinor.Sign() <= 0 {
			return bad("github_user_id %d has a non-positive amount", l.GitHubUserID)
		}
		if l.Status != StatusPayable && l.Status != StatusHeldKYC {
			return bad("github_user_id %d has unknown status %q", l.GitHubUserID, l.Status)
		}
		sum.Add(sum, l.AmountMinor)
	}
	if sum.Cmp(s.PoolMinor) != 0 {
		return bad("lines sum to %s but pool_minor is %s", sum, s.PoolMinor)
	}
	return nil
}

// writeCanonical writes v as JSON with object keys sorted. It accepts only the
// shapes a statement is built from, so nothing reaches the output by a path
// whose formatting has not been decided here.
func writeCanonical(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case string:
		return writeString(b, x)
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// Byte order. Every key is ASCII, where this is the same order as
		// JavaScript's default sort (UTF-16 code units).
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeString(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("%w: cannot encode %T", ErrInvalidStatement, v)
	}
	return nil
}

// writeString escapes exactly as JSON.stringify does: the two-character forms
// for \b \t \n \f \r " and \\, \u00xx (lowercase hex) for every other control
// character below U+0020, and everything else - including <, >, &, U+2028 and
// U+2029, which Go's encoding/json would escape - written as itself. Invalid
// UTF-8 is refused: JavaScript strings cannot carry it, so the two sides could
// never agree on its bytes.
func writeString(b *strings.Builder, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: string is not valid UTF-8", ErrInvalidStatement)
	}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return nil
}

// ParseSigningKey reads GRAINHACK_RESULTS_SIGNING_KEY: base64 (standard, with
// padding) of a 32-byte Ed25519 seed - the same encoding as
// BOUNTY_LINK_SIGNING_KEY. Empty returns (nil, nil): the feature is off.
func ParseSigningKey(b64 string) (ed25519.PrivateKey, error) {
	raw := strings.TrimSpace(b64)
	if raw == "" {
		return nil, nil
	}
	seed, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("GRAINHACK_RESULTS_SIGNING_KEY is not base64 of a 32-byte ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// PublicKeyB64 is the public half as GRAINHACK_RESULTS_PUBKEY expects it:
// base64 (standard) of the 32-byte key.
func PublicKeyB64(key ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
}

// Sign signs the UTF-8 bytes of SignatureDomain + canonical and returns the
// 64-byte signature in standard base64 - the encoding the wallet-link
// countersignature uses.
func Sign(key ed25519.PrivateKey, canonical string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(SignatureDomain+canonical)))
}

// Verify checks a signature produced by Sign.
func Verify(pub ed25519.PublicKey, canonical, signatureB64 string) bool {
	sig, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil || len(sig) != ed25519.SignatureSize || len(pub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(pub, []byte(SignatureDomain+canonical), sig)
}

// SHA256Hex is sha256 of the canonical statement as lowercase hex: the
// statement_sha256 an approval's terms carry.
func SHA256Hex(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}
