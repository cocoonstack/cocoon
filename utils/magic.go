package utils

// Magic byte prefixes shared by image/blob format sniffing.
var (
	// Qcow2Magic is the qcow2 file signature ("QFI\xfb").
	Qcow2Magic = []byte{'Q', 'F', 'I', 0xfb}

	// GzipMagic is the gzip stream signature.
	GzipMagic = []byte{0x1f, 0x8b}

	// ZstdMagic is the zstd frame signature.
	ZstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}
)
