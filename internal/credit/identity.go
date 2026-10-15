package credit

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	principalPrefix = "cmk:"
	materialPrefix  = "sk-"
	materialChars   = 48
	// materialMinLen and materialMaxLen bound caller supplied key material.
	// The upper bound also keeps lookup cheap: rejecting an oversized value
	// before hashing prevents an unauthenticated HMAC denial of service.
	materialMinLen = 8
	materialMaxLen = 4096
	alphabet       = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	fingerprintLen = 16
)

func readRandom(b []byte) (int, error) {
	n, err := rand.Read(b)
	if err != nil {
		return n, fmt.Errorf("random source unavailable: %w", err)
	}
	return n, nil
}

// newUUID returns a random RFC 4122 version 4 identifier.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := readRandom(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:]), nil
}

// newMaterial returns "sk-" followed by 48 unbiased alphanumeric characters.
func newMaterial() (string, error) {
	var sb strings.Builder
	sb.WriteString(materialPrefix)
	buf := make([]byte, materialChars)
	out := make([]byte, 0, materialChars)
	// Rejection sampling keeps the distribution uniform: 248 = 62*4.
	for len(out) < materialChars {
		if _, err := readRandom(buf); err != nil {
			return "", err
		}
		for _, v := range buf {
			if v >= 248 {
				continue
			}
			out = append(out, alphabet[int(v)%len(alphabet)])
			if len(out) == materialChars {
				break
			}
		}
	}
	sb.Write(out)
	return sb.String(), nil
}

// callerScopeSalt is the host namespace salt. CPA derives the trusted
// caller_scope value as hex(sha256(salt + principal)). The derivation is
// duplicated here with stdlib crypto so this package keeps the host's identity
// stable without depending on the SDK session graph.
const callerScopeSalt = "cli-proxy-api:caller-scope:v1\x00"

// callerScope mirrors session.CallerScope of the host SDK. The scope is an
// irreversible identity, never a secret, and it must match what the host puts
// into caller_scope metadata or every request would fail authentication.
func callerScope(principal string) string {
	value := strings.TrimSpace(principal)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(callerScopeSalt + value))
	return hex.EncodeToString(sum[:])
}

// validateMaterial checks caller supplied key material. CPA trims the value on
// its way through the host, so surrounding whitespace could never round trip
// and is rejected instead of being silently rewritten. The prefix and character
// set stay free form: only NUL/control characters and broken UTF-8 are refused.
func validateMaterial(material string) error {
	if material == "" {
		return fmt.Errorf("%w: key material is empty", ErrInvalid)
	}
	if len(material) < materialMinLen {
		return fmt.Errorf("%w: key material is too short", ErrInvalid)
	}
	if len(material) > materialMaxLen {
		return fmt.Errorf("%w: key material is too long", ErrInvalid)
	}
	if strings.TrimSpace(material) != material {
		return fmt.Errorf("%w: key material must not have leading or trailing whitespace", ErrInvalid)
	}
	if !utf8.ValidString(material) {
		return fmt.Errorf("%w: key material must be valid UTF-8", ErrInvalid)
	}
	for _, r := range material {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: key material must not contain control characters", ErrInvalid)
		}
	}
	return nil
}

// materialHMAC is the irreversible key verifier. Plain material is never stored.
func materialHMAC(pepper []byte, material string) string {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(material))
	return hex.EncodeToString(mac.Sum(nil))
}

func fingerprintOf(hmac string) string {
	if len(hmac) < fingerprintLen {
		return hmac
	}
	return hmac[:fingerprintLen]
}
