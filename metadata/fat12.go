package metadata

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	sectorSize     = 512
	totalSectors   = 2048 // 1 MiB
	sectorsPerClus = 1
	reservedSec    = 1
	numFATs        = 2
	sectorsPerFAT  = 6
	rootEntryCount = 128
	firstDataSec   = reservedSec + numFATs*sectorsPerFAT + rootDirSectors
	rootDirSectors = rootEntryCount * dirEntrySize / sectorSize
	dirEntrySize   = 32
	fatEntryEOC    = 0xFFF
	mediaDesc      = 0xF8

	writeBufSize = 64 << 10
)

type fat12Builder struct {
	label       string
	fat         []byte // single FAT copy (written twice)
	rootDir     []byte
	data        [][]byte // file data in cluster-allocation order
	nextCluster uint16
	rootUsed    int
	shortSeq    int
}

func newFAT12Builder(label string) *fat12Builder {
	b := &fat12Builder{
		label:       label,
		fat:         make([]byte, sectorsPerFAT*sectorSize),
		rootDir:     make([]byte, rootEntryCount*dirEntrySize),
		nextCluster: 2,
	}
	// Clusters 0 and 1 are reserved by the FAT spec
	setFATEntry(b.fat, 0, 0xFF8)
	setFATEntry(b.fat, 1, fatEntryEOC)
	b.addVolumeLabel()
	return b
}

func (b *fat12Builder) addVolumeLabel() {
	name := padLabel(b.label)
	off, _ := b.writeDirEntry(name[:])
	b.rootDir[off+11] = 0x08
	putTimestamps(b.rootDir[off:], time.Now())
}

func (b *fat12Builder) addFile(name string, content []byte) error {
	numClusters := (len(content) + sectorSize - 1) / sectorSize

	var startCluster uint16
	if numClusters > 0 {
		if int(b.nextCluster)+numClusters > (totalSectors-firstDataSec)+2 {
			return fmt.Errorf("fat12: not enough space for %s", name)
		}
		startCluster = b.nextCluster
		for i := range numClusters {
			c := int(b.nextCluster) + i
			if i == numClusters-1 {
				setFATEntry(b.fat, c, fatEntryEOC)
			} else {
				setFATEntry(b.fat, c, uint16(c+1)) //nolint:gosec
			}
		}
		b.data = append(b.data, content)
		b.nextCluster += uint16(numClusters) //nolint:gosec
	}

	var shortName [11]byte
	if needsLFN(name) {
		b.shortSeq++
		shortName = generateShortName(name, b.shortSeq)
		for _, entry := range makeLFNEntries(name, shortName) {
			if _, err := b.writeDirEntry(entry); err != nil {
				return err
			}
		}
	} else {
		shortName = toShortName(name)
	}

	off, err := b.writeDirEntry(shortName[:])
	if err != nil {
		return err
	}
	b.rootDir[off+11] = 0x20 // archive
	putTimestamps(b.rootDir[off:], time.Now())
	binary.LittleEndian.PutUint16(b.rootDir[off+26:], startCluster)
	binary.LittleEndian.PutUint32(b.rootDir[off+28:], uint32(len(content))) //nolint:gosec
	return nil
}

func (b *fat12Builder) writeDirEntry(entry []byte) (int, error) {
	if b.rootUsed >= rootEntryCount {
		return 0, fmt.Errorf("fat12: root directory full")
	}
	off := b.rootUsed * dirEntrySize
	copy(b.rootDir[off:], entry)
	b.rootUsed++
	return off, nil
}

func (b *fat12Builder) writeTo(w io.Writer) error {
	bw := bufio.NewWriterSize(w, writeBufSize)
	if _, err := bw.Write(b.makeBootSector()); err != nil {
		return err
	}
	for range numFATs {
		if _, err := bw.Write(b.fat); err != nil {
			return err
		}
	}
	if _, err := bw.Write(b.rootDir); err != nil {
		return err
	}

	sector := make([]byte, sectorSize)
	for _, content := range b.data {
		for chunk := range slices.Chunk(content, sectorSize) {
			clear(sector)
			copy(sector, chunk)
			if _, err := bw.Write(sector); err != nil {
				return err
			}
		}
	}

	clear(sector)
	for range totalSectors - firstDataSec - (int(b.nextCluster) - 2) {
		if _, err := bw.Write(sector); err != nil {
			return err
		}
	}
	return bw.Flush()
}

func (b *fat12Builder) makeBootSector() []byte {
	boot := make([]byte, sectorSize)

	// x86 jump + NOP
	boot[0], boot[1], boot[2] = 0xEB, 0x3C, 0x90

	copy(boot[3:], "COCOON  ")
	binary.LittleEndian.PutUint16(boot[11:], sectorSize)
	boot[13] = sectorsPerClus
	binary.LittleEndian.PutUint16(boot[14:], reservedSec)
	boot[16] = numFATs
	binary.LittleEndian.PutUint16(boot[17:], rootEntryCount)
	binary.LittleEndian.PutUint16(boot[19:], totalSectors)
	boot[21] = mediaDesc
	binary.LittleEndian.PutUint16(boot[22:], sectorsPerFAT)
	binary.LittleEndian.PutUint16(boot[24:], 32)                            // sectors per track
	binary.LittleEndian.PutUint16(boot[26:], 64)                            // heads
	boot[36] = 0x80                                                         // drive number
	boot[38] = 0x29                                                         // extended boot signature
	binary.LittleEndian.PutUint32(boot[39:], uint32(time.Now().UnixNano())) //nolint:gosec

	label := padLabel(b.label)
	copy(boot[43:54], label[:])
	copy(boot[54:62], "FAT12   ")
	boot[510], boot[511] = 0x55, 0xAA
	return boot
}

// CreateFAT12 streams a 1 MiB FAT12 image with VFAT long-filename support to w.
func CreateFAT12(w io.Writer, label string, files map[string][]byte) error {
	b := newFAT12Builder(label)

	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := b.addFile(name, files[name]); err != nil {
			return err
		}
	}
	return b.writeTo(w)
}

func setFATEntry(fat []byte, cluster int, val uint16) {
	off := cluster + cluster/2
	word := uint16(fat[off]) | uint16(fat[off+1])<<8
	if cluster%2 == 0 {
		word = (word & 0xF000) | (val & 0x0FFF)
	} else {
		word = (word & 0x000F) | ((val & 0x0FFF) << 4)
	}
	fat[off] = byte(word) //nolint:gosec
	fat[off+1] = byte(word >> 8)
}

func needsLFN(name string) bool {
	upper := strings.ToUpper(name)
	base, ext := splitName(upper)
	return len(base) > 8 || len(ext) > 3 || name != upper || strings.Count(name, ".") > 1
}

func blankSFN() [11]byte {
	return [11]byte{' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}
}

func splitName(upper string) (base, ext string) {
	if base, ext, ok := strings.CutLast(upper, "."); ok {
		return base, ext
	}
	return upper, ""
}

// toShortName pads a simple name (already fits 8.3, uppercase) into an 11-byte SFN.
func toShortName(name string) [11]byte {
	result := blankSFN()
	base, ext := splitName(strings.ToUpper(name))
	copy(result[:8], base)
	copy(result[8:], ext)
	return result
}

// generateShortName builds an 8.3 name with numeric tail (e.g. "META-D~1   ").
func generateShortName(name string, seq int) [11]byte {
	result := blankSFN()

	base, ext := splitName(strings.ToUpper(name))
	base = strings.ReplaceAll(base, ".", "")

	tail := fmt.Sprintf("~%d", seq)
	maxBase := 8 - len(tail)
	copy(result[:8], base[:min(len(base), maxBase)]+tail)
	copy(result[8:], ext)
	return result
}

// makeLFNEntries creates VFAT long-filename directory entries in disk order (highest sequence number first, immediately before the SFN entry).
func makeLFNEntries(name string, shortName [11]byte) [][]byte {
	runes := utf16.Encode([]rune(name))
	chksum := lfnChecksum(shortName)
	numEntries := (len(runes) + 12) / 13

	entries := make([][]byte, numEntries)
	for i := range numEntries {
		entry := make([]byte, dirEntrySize)

		seq := byte(i + 1)
		if i == numEntries-1 {
			seq |= 0x40
		}
		entry[0] = seq
		entry[11] = 0x0F // LFN attribute
		entry[13] = chksum

		base := i * 13
		putLFNChars(entry[1:11], runes, base, 5)
		putLFNChars(entry[14:26], runes, base+5, 6)
		putLFNChars(entry[28:32], runes, base+11, 2)

		entries[i] = entry
	}

	slices.Reverse(entries)
	return entries
}

// putLFNChars fills UCS-2 character slots: data char, then null terminator, then 0xFFFF padding.
func putLFNChars(dst []byte, runes []uint16, offset, count int) {
	for j := range count {
		idx := offset + j
		pos := j * 2
		switch {
		case idx < len(runes):
			binary.LittleEndian.PutUint16(dst[pos:], runes[idx])
		case idx == len(runes):
			// Null terminator
		default:
			binary.LittleEndian.PutUint16(dst[pos:], 0xFFFF)
		}
	}
}

func lfnChecksum(shortName [11]byte) byte {
	var sum byte
	for _, b := range shortName {
		sum = ((sum >> 1) | (sum << 7)) + b
	}
	return sum
}

func padLabel(label string) [11]byte {
	result := blankSFN()
	copy(result[:], strings.ToUpper(label))
	return result
}

func putTimestamps(entry []byte, t time.Time) {
	date, tm := encodeFATDateTime(t)
	binary.LittleEndian.PutUint16(entry[14:], tm)
	binary.LittleEndian.PutUint16(entry[16:], date)
	binary.LittleEndian.PutUint16(entry[18:], date) // last access
	binary.LittleEndian.PutUint16(entry[22:], tm)
	binary.LittleEndian.PutUint16(entry[24:], date)
}

func encodeFATDateTime(t time.Time) (date, fatTime uint16) {
	date = uint16((t.Year()-1980)<<9) | uint16(int(t.Month())<<5) | uint16(t.Day()) //nolint:gosec
	fatTime = uint16(t.Hour()<<11) | uint16(t.Minute()<<5) | uint16(t.Second()/2)   //nolint:gosec
	return date, fatTime
}
