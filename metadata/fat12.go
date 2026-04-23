package metadata

import (
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
)

// FAT12 disk layout constants for a 1 MiB image.
const (
	sectorSize     = 512
	totalSectors   = 2048 // 1 MiB
	sectorsPerClus = 1
	reservedSec    = 1
	numFATs        = 2
	sectorsPerFAT  = 6
	rootEntryCount = 128
	firstDataSec   = reservedSec + numFATs*sectorsPerFAT + rootDirSectors // 21
	rootDirSectors = rootEntryCount * dirEntrySize / sectorSize           // 8
	dirEntrySize   = 32
	fatEntryEOC    = 0xFFF
	mediaDesc      = 0xF8
)

type dataEntry struct {
	data        []byte
	numClusters int
}

// fat12Builder constructs a FAT12 image in memory (FAT + root dir only)
// and streams the full image on writeTo.
type fat12Builder struct {
	label       string
	fat         []byte      // single FAT copy (written twice)
	rootDir     []byte      // root directory area
	data        []dataEntry // file data in cluster-allocation order
	nextCluster uint16      // next free cluster (starts at 2)
	rootUsed    int         // root directory entries consumed
	shortSeq    int         // counter for ~N short-name suffixes
}

// CreateFAT12 streams a 1 MiB FAT12 image with VFAT long-filename support to w.
// label is the volume label (e.g. "CIDATA"); files maps filename → content.
func CreateFAT12(w io.Writer, label string, files map[string][]byte) error {
	b := newFAT12Builder(label)

	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := b.addFile(name, files[name]); err != nil {
			return err
		}
	}
	return b.writeTo(w)
}

func newFAT12Builder(label string) *fat12Builder {
	b := &fat12Builder{
		label:       label,
		fat:         make([]byte, sectorsPerFAT*sectorSize),
		rootDir:     make([]byte, rootEntryCount*dirEntrySize),
		nextCluster: 2, //nolint:mnd
	}
	// Reserved FAT entries.
	setFATEntry(b.fat, 0, 0xFF8) //nolint:mnd
	setFATEntry(b.fat, 1, fatEntryEOC)
	b.addVolumeLabel()
	return b
}

// addVolumeLabel writes the volume-label directory entry (attribute 0x08).
func (b *fat12Builder) addVolumeLabel() {
	name := padLabel(b.label)
	off := b.rootUsed * dirEntrySize
	copy(b.rootDir[off:], name[:])
	b.rootDir[off+11] = 0x08 //nolint:mnd
	putTimestamps(b.rootDir[off:], time.Now())
	b.rootUsed++
}

// addFile registers a file: allocates FAT clusters, writes LFN + SFN directory entries.
func (b *fat12Builder) addFile(name string, content []byte) error {
	numClusters := (len(content) + sectorSize - 1) / sectorSize

	var startCluster uint16
	if numClusters > 0 {
		if int(b.nextCluster)+numClusters > (totalSectors-firstDataSec)+2 { //nolint:mnd
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
		b.data = append(b.data, dataEntry{data: content, numClusters: numClusters})
		b.nextCluster += uint16(numClusters)
	}

	// Generate 8.3 short name — with ~N suffix when LFN is needed.
	lfn := needsLFN(name)
	var shortName [11]byte
	if lfn {
		b.shortSeq++
		shortName = generateShortName(name, b.shortSeq)
		lfnEntries := makeLFNEntries(name, shortName)
		for _, entry := range lfnEntries {
			if b.rootUsed >= rootEntryCount {
				return fmt.Errorf("fat12: root directory full")
			}
			off := b.rootUsed * dirEntrySize
			copy(b.rootDir[off:], entry)
			b.rootUsed++
		}
	} else {
		shortName = toShortName(name)
	}

	if b.rootUsed >= rootEntryCount {
		return fmt.Errorf("fat12: root directory full")
	}
	off := b.rootUsed * dirEntrySize
	copy(b.rootDir[off:], shortName[:])
	b.rootDir[off+11] = 0x20 // archive //nolint:mnd
	putTimestamps(b.rootDir[off:], time.Now())
	binary.LittleEndian.PutUint16(b.rootDir[off+26:], startCluster)         //nolint:mnd
	binary.LittleEndian.PutUint32(b.rootDir[off+28:], uint32(len(content))) //nolint:mnd,gosec
	b.rootUsed++
	return nil
}

// writeTo streams: boot sector → FAT ×2 → root directory → data → zero padding.
func (b *fat12Builder) writeTo(w io.Writer) error {
	if _, err := w.Write(b.makeBootSector()); err != nil {
		return err
	}
	for range numFATs {
		if _, err := w.Write(b.fat); err != nil {
			return err
		}
	}
	if _, err := w.Write(b.rootDir); err != nil {
		return err
	}

	sector := make([]byte, sectorSize)
	dataSectors := 0
	for _, e := range b.data {
		for i := range e.numClusters {
			clear(sector)
			start := i * sectorSize
			if start < len(e.data) {
				copy(sector, e.data[start:min(start+sectorSize, len(e.data))])
			}
			if _, err := w.Write(sector); err != nil {
				return err
			}
			dataSectors++
		}
	}

	// Zero-fill the remaining data area.
	clear(sector)
	for range totalSectors - firstDataSec - dataSectors {
		if _, err := w.Write(sector); err != nil {
			return err
		}
	}
	return nil
}

func (b *fat12Builder) makeBootSector() []byte {
	boot := make([]byte, sectorSize)

	// x86 jump + NOP
	boot[0], boot[1], boot[2] = 0xEB, 0x3C, 0x90 //nolint:mnd

	copy(boot[3:], "COCOON  ")                                              //nolint:mnd
	binary.LittleEndian.PutUint16(boot[11:], sectorSize)                    //nolint:mnd
	boot[13] = sectorsPerClus                                               //nolint:mnd
	binary.LittleEndian.PutUint16(boot[14:], reservedSec)                   //nolint:mnd
	boot[16] = numFATs                                                      //nolint:mnd
	binary.LittleEndian.PutUint16(boot[17:], rootEntryCount)                //nolint:mnd
	binary.LittleEndian.PutUint16(boot[19:], totalSectors)                  //nolint:mnd
	boot[21] = mediaDesc                                                    //nolint:mnd
	binary.LittleEndian.PutUint16(boot[22:], sectorsPerFAT)                 //nolint:mnd
	binary.LittleEndian.PutUint16(boot[24:], 32)                            // sectors per track //nolint:mnd
	binary.LittleEndian.PutUint16(boot[26:], 64)                            // heads //nolint:mnd
	boot[36] = 0x80                                                         // drive number //nolint:mnd
	boot[38] = 0x29                                                         // extended boot signature //nolint:mnd
	binary.LittleEndian.PutUint32(boot[39:], uint32(time.Now().UnixNano())) //nolint:mnd,gosec

	label := padLabel(b.label)
	copy(boot[43:54], label[:])       //nolint:mnd
	copy(boot[54:62], "FAT12   ")     //nolint:mnd
	boot[510], boot[511] = 0x55, 0xAA //nolint:mnd
	return boot
}

// --- FAT12 entry encoding ---

// setFATEntry writes a 12-bit value into the FAT at the given cluster index.
func setFATEntry(fat []byte, cluster int, val uint16) {
	off := cluster + cluster/2 //nolint:mnd
	if off+1 >= len(fat) {
		return
	}
	word := uint16(fat[off]) | uint16(fat[off+1])<<8
	if cluster%2 == 0 { //nolint:mnd
		word = (word & 0xF000) | (val & 0x0FFF) //nolint:mnd
	} else {
		word = (word & 0x000F) | ((val & 0x0FFF) << 4) //nolint:mnd
	}
	fat[off] = byte(word)
	fat[off+1] = byte(word >> 8) //nolint:mnd
}

// --- directory helpers ---

// needsLFN reports whether name requires VFAT long-filename entries.
func needsLFN(name string) bool {
	upper := strings.ToUpper(name)
	var base, ext string
	if dot := strings.LastIndex(upper, "."); dot >= 0 {
		base = upper[:dot]
		ext = upper[dot+1:]
	} else {
		base = upper
	}
	return len(base) > 8 || len(ext) > 3 || name != upper || strings.Count(name, ".") > 1 //nolint:mnd
}

// toShortName pads a simple name (already fits 8.3, uppercase) into an 11-byte SFN.
func toShortName(name string) [11]byte {
	var result [11]byte
	for i := range result {
		result[i] = ' '
	}
	upper := strings.ToUpper(name)
	if dot := strings.LastIndex(upper, "."); dot >= 0 {
		copy(result[:8], upper[:dot])   //nolint:mnd
		copy(result[8:], upper[dot+1:]) //nolint:mnd
	} else {
		copy(result[:8], upper) //nolint:mnd
	}
	return result
}

// generateShortName builds an 8.3 name with numeric tail (e.g. "META-D~1   ").
func generateShortName(name string, seq int) [11]byte {
	var result [11]byte
	for i := range result {
		result[i] = ' '
	}

	upper := strings.ToUpper(name)
	var base, ext string
	if dot := strings.LastIndex(upper, "."); dot >= 0 {
		base = strings.ReplaceAll(upper[:dot], ".", "")
		ext = upper[dot+1:]
	} else {
		base = upper
	}

	tail := fmt.Sprintf("~%d", seq)
	maxBase := 8 - len(tail) //nolint:mnd
	if len(base) > maxBase {
		base = base[:maxBase]
	}
	copy(result[:8], base+tail) //nolint:mnd
	if len(ext) > 3 {           //nolint:mnd
		ext = ext[:3] //nolint:mnd
	}
	copy(result[8:], ext) //nolint:mnd
	return result
}

// --- VFAT LFN ---

// makeLFNEntries creates VFAT long-filename directory entries in disk order
// (highest sequence number first, immediately before the SFN entry).
func makeLFNEntries(name string, shortName [11]byte) [][]byte {
	runes := utf16.Encode([]rune(name))
	chksum := lfnChecksum(shortName)
	numEntries := (len(runes) + 12) / 13 //nolint:mnd

	entries := make([][]byte, numEntries)
	for i := range numEntries {
		entry := make([]byte, dirEntrySize)

		seq := byte(i + 1)
		if i == numEntries-1 {
			seq |= 0x40 //nolint:mnd
		}
		entry[0] = seq
		entry[11] = 0x0F   // LFN attribute //nolint:mnd
		entry[13] = chksum //nolint:mnd

		base := i * 13                               //nolint:mnd
		putLFNChars(entry[1:11], runes, base, 5)     //nolint:mnd
		putLFNChars(entry[14:26], runes, base+5, 6)  //nolint:mnd
		putLFNChars(entry[28:32], runes, base+11, 2) //nolint:mnd

		entries[i] = entry
	}

	slices.Reverse(entries)
	return entries
}

// putLFNChars fills UCS-2 character slots: data char, then null terminator, then 0xFFFF padding.
func putLFNChars(dst []byte, runes []uint16, offset, count int) {
	for j := range count {
		idx := offset + j
		pos := j * 2 //nolint:mnd
		switch {
		case idx < len(runes):
			binary.LittleEndian.PutUint16(dst[pos:], runes[idx])
		case idx == len(runes):
			// null terminator
		default:
			binary.LittleEndian.PutUint16(dst[pos:], 0xFFFF) //nolint:mnd
		}
	}
}

func lfnChecksum(shortName [11]byte) byte {
	var sum byte
	for _, b := range shortName {
		sum = ((sum >> 1) | (sum << 7)) + b //nolint:mnd
	}
	return sum
}

// --- timestamp helpers ---

func padLabel(label string) [11]byte {
	var result [11]byte
	for i := range result {
		result[i] = ' '
	}
	copy(result[:], strings.ToUpper(label))
	return result
}

func putTimestamps(entry []byte, t time.Time) {
	date, tm := encodeFATDateTime(t)
	binary.LittleEndian.PutUint16(entry[14:], tm)   //nolint:mnd
	binary.LittleEndian.PutUint16(entry[16:], date) //nolint:mnd
	binary.LittleEndian.PutUint16(entry[18:], date) // last access //nolint:mnd
	binary.LittleEndian.PutUint16(entry[22:], tm)   //nolint:mnd
	binary.LittleEndian.PutUint16(entry[24:], date) //nolint:mnd
}

func encodeFATDateTime(t time.Time) (date, fatTime uint16) {
	date = uint16((t.Year()-1980)<<9) | uint16(int(t.Month())<<5) | uint16(t.Day()) //nolint:mnd,gosec
	fatTime = uint16(t.Hour()<<11) | uint16(t.Minute()<<5) | uint16(t.Second()/2)   //nolint:mnd,gosec
	return date, fatTime
}
