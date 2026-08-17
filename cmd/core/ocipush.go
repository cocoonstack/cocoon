package core

import (
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	commonoci "github.com/cocoonstack/cocoon-common/oci"
)

// OCIPushTarget is a fully-qualified, tag-addressed OCI destination split for cocoon-common's registry client.
type OCIPushTarget struct {
	Registry   *commonoci.OCIRegistry
	Repository string
	Tag        string
}

// ParseOCIPushTarget parses registry/repository[:tag]; digest destinations cannot name a manifest that has not been created yet.
func ParseOCIPushTarget(raw string) (*OCIPushTarget, error) {
	ref, err := name.ParseReference(raw, name.StrictValidation)
	if err != nil {
		repo, repoErr := name.NewRepository(raw, name.StrictValidation)
		if repoErr != nil {
			return nil, fmt.Errorf("parse OCI destination %q: %w", raw, err)
		}
		ref = repo.Tag("latest")
	}
	if _, ok := ref.(name.Tag); !ok {
		return nil, fmt.Errorf("OCI destination %q must use a tag, not a digest", raw)
	}
	repo := ref.Context()
	return &OCIPushTarget{
		Registry:   commonoci.NewOCIRegistry(repo.RegistryStr(), authn.DefaultKeychain),
		Repository: repo.RepositoryStr(),
		Tag:        ref.Identifier(),
	}, nil
}
