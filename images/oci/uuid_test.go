package oci

import "testing"

func TestUUIDV5Vector(t *testing.T) {
	const want = "767acf47-c1e4-5cf8-a34c-45a7096faa2d"
	if got := uuidV5("test-name"); got != want {
		t.Fatalf("uuidV5(%q) = %s, want %s", "test-name", got, want)
	}
}
