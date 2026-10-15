package credit

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	principalPrefix = "cmk:"
	scopePrefix     = "cms:"
	materialPrefix  = "sk-"
	materialChars   = 48
	alphabet        = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	fingerprintLen  = 16
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
