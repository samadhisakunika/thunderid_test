package controller

import (
	"crypto/rand"
	"encoding/hex"
)

// randomUsername generates a short, readable, random username for an
// auto-provisioned credentials Secret.
func randomUsername() ([]byte, error) {
	suffix, err := randomHex(4)
	if err != nil {
		return nil, err
	}
	return append([]byte("user-"), suffix...), nil
}

// randomPassword generates a short, random password for an auto-provisioned
// credentials Secret — meant to be typed during initial login, not cryptographic
// key material, so it's deliberately much shorter than randomHexSecret's 32 bytes.
func randomPassword() ([]byte, error) {
	return randomHex(4)
}

// randomHex returns n random bytes, hex-encoded (2n characters).
func randomHex(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return []byte(hex.EncodeToString(buf)), nil
}
