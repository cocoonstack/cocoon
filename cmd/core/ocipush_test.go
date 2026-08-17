package core

import "testing"

func TestParseOCIPushTarget(t *testing.T) {
	target, err := ParseOCIPushTarget("registry.example.com/team/image:v1")
	if err != nil {
		t.Fatal(err)
	}
	if target.Repository != "team/image" || target.Tag != "v1" {
		t.Fatalf("target = repo %q tag %q", target.Repository, target.Tag)
	}
}

func TestParseOCIPushTargetDefaultsToLatest(t *testing.T) {
	target, err := ParseOCIPushTarget("registry.example.com/team/image")
	if err != nil {
		t.Fatal(err)
	}
	if target.Repository != "team/image" || target.Tag != "latest" {
		t.Fatalf("target = repo %q tag %q", target.Repository, target.Tag)
	}
}

func TestParseOCIPushTargetRejectsDigest(t *testing.T) {
	if _, err := ParseOCIPushTarget("registry.example.com/team/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("expected digest destination to be rejected")
	}
}
