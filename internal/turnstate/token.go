// Package turnstate treats encrypted state as opaque. Shape is a heuristic,
// never a signature check or a measurement of model quality.
package turnstate

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"
)

const Header = "X-Codex-Turn-State"

type Policy struct {
	Blocks       []int
	TTL, Refresh time.Duration
}
type Token struct {
	Value, Fingerprint string
	Issued             time.Time
	Blocks             int
}

func Parse(value string) (Token, error) {
	var t Token
	value = strings.TrimSpace(value)
	if len(value) > 2048 || strings.ContainsAny(value, "\r\n\t ") {
		return t, errors.New("invalid state encoding")
	}
	core := strings.TrimRight(value, "=")
	if len(value)-len(core) > 2 {
		return t, errors.New("invalid state padding")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(core)
	if err != nil || len(raw) < 73 || raw[0] != 0x80 || (len(raw)-57)%16 != 0 {
		return t, errors.New("unrecognized state envelope")
	}
	issued := binary.BigEndian.Uint64(raw[1:9])
	if issued < 1577836800 || issued >= 4102444800 {
		return t, errors.New("state timestamp out of range")
	}
	sum := sha256.Sum256([]byte(value))
	return Token{value, hex.EncodeToString(sum[:8]), time.Unix(int64(issued), 0), (len(raw) - 57) / 16}, nil
}

func (p Policy) Accept(t Token, now time.Time) bool {
	return t.Value != "" && slices.Contains(p.Blocks, t.Blocks) && !t.Issued.After(now.Add(30*time.Second)) && now.Before(t.Issued.Add(p.TTL-30*time.Second))
}
