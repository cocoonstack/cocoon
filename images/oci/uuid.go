package oci

import "github.com/google/uuid"

// uuidV5 returns the RFC 4122 version 5 UUID of name in the URL namespace.
func uuidV5(name string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(name)).String()
}
