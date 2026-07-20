package cloudimg

import (
	"time"

	"github.com/cocoonstack/cocoon/images"
)

type imageIndex = images.Index[imageEntry]

type imageEntry struct {
	Ref        string        `json:"ref"`
	ContentSum images.Digest `json:"content_sum"`
	Size       int64         `json:"size"`
	CreatedAt  time.Time     `json:"created_at"`
}

func (e imageEntry) EntryID() string           { return e.ContentSum.String() }
func (e imageEntry) EntryRef() string          { return e.Ref }
func (e imageEntry) EntryCreatedAt() time.Time { return e.CreatedAt }
func (e imageEntry) DigestHexes() []string     { return []string{e.ContentSum.Hex()} }
