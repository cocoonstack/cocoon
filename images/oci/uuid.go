package oci

import "github.com/google/uuid"

// uuidV5 generates a deterministic UUID v5 from the given name using the URL namespace. Compatible with the uuid_v5() bash function in os-image/start.sh.
func uuidV5(name string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(name)).String()
}
