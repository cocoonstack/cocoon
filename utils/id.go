package utils

import (
	"crypto/rand"
)

// GenerateID returns a random 26-character crypto-secure base32 ID.
func GenerateID() string {
	return rand.Text()
}
