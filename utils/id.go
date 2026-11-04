package utils

import (
	"crypto/rand"
	"strings"
)

const idLength = 26

// GenerateID returns a random 26-character crypto-secure base32 ID.
func GenerateID() string {
	return rand.Text()
}

// ValidID reports whether id has the shape GenerateID produces.
func ValidID(id string) bool {
	return len(id) == idLength && !strings.ContainsFunc(id, func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < '2' || r > '7')
	})
}
