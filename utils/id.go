package utils

import (
	"crypto/rand"
	"regexp"
)

var validID = regexp.MustCompile(`^[A-Z2-7]{26}$`)

// GenerateID returns a random 26-character crypto-secure base32 ID.
func GenerateID() string {
	return rand.Text()
}

// ValidID reports whether id has the shape GenerateID produces.
func ValidID(id string) bool {
	return validID.MatchString(id)
}
