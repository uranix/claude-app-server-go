// Package idgen generates random identifiers: UUIDv4 for claude CLI session
// IDs (which the CLI requires to look UUID-shaped) and short hex IDs for
// threads, turns, and items.
package idgen

import "crypto/rand"

// UUIDv4 returns a random RFC 4122 version-4 UUID string.
func UUIDv4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0F) | 0x40 // version 4
	b[8] = (b[8] & 0x3F) | 0x80 // variant 10

	return string(hexDigits(b[0:4])) + "-" +
		string(hexDigits(b[4:6])) + "-" +
		string(hexDigits(b[6:8])) + "-" +
		string(hexDigits(b[8:10])) + "-" +
		string(hexDigits(b[10:16]))
}

// Hex returns n random bytes rendered as a lowercase hex string (2n chars).
func Hex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return string(hexDigits(b))
}

const hexAlphabet = "0123456789abcdef"

func hexDigits(b []byte) []byte {
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexAlphabet[c>>4]
		out[i*2+1] = hexAlphabet[c&0x0F]
	}
	return out
}
