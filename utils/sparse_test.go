package utils

import (
	"testing"
)

func TestSparseCopy_FileCopy(t *testing.T) {
	testFileCopy(t, SparseCopy)
}
