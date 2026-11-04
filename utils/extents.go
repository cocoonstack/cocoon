package utils

// Extent maps a file range to its on-disk location.
type Extent struct {
	Logical   int64
	Physical  int64
	Length    int64
	Unwritten bool
}
