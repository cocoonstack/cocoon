package oci

import "github.com/google/uuid"

func uuidV5(name string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(name)).String()
}
