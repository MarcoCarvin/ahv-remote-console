// Deterministic-output pass: go-diskfs stamps wall-clock time into the ISO
// (PVD/SVD volume dates, every directory record's recording date, and Rock
// Ridge TF timestamps) with no option to override, so two builds from
// identical inputs differ in dozens of timestamp bytes. That breaks the
// catalog dedupe: the uploaded image is named after the ISO's SHA-256, so
// timestamp churn re-uploads a "new" image on every rebuild — the known
// catalog-growth failure mode.
//
// normalizeTimestamps rewrites exactly those timestamp fields in-place to a
// fixed instant (the git commit date, passed via -mtime). Directories are
// enumerated from the PATH TABLES, not by walking the directory tree: the
// go-diskfs Joliet tree contains subdirectory records that point at the
// ISO9660 extents, leaving the real Joliet subdirectory extents orphaned
// from the tree (but still listed in the Joliet path table — and still
// carrying churning timestamps). Every write is a same-length overwrite at a
// structurally-derived offset, so the ISO layout is untouched. Verified by
// the determinism test: identical inputs + same -mtime → byte-identical ISOs.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"
)

const (
	pvdSector = 16 // primary volume descriptor
	svdSector = 17 // supplementary (Joliet) volume descriptor

	// Offsets inside a volume descriptor sector (ECMA-119 §8.4 / §8.5).
	vdRootRecordOff   = 156 // 34-byte root directory record
	vdPathTableSize   = 132 // both-endian uint32
	vdPathTableLOff   = 140 // little-endian path table location (uint32 LE)
	vdCreationOff     = 813 // 17-byte dec-datetime
	vdModifiedOff     = 830
	vdExpirationOff   = 847
	vdEffectiveOff    = 864
	vdDecDatetimeSize = 17

	// Offsets inside a directory record (ECMA-119 §9.1).
	drExtentOff = 2  // extent LBA, both-endian (LE first)
	drSizeOff   = 10 // extent size, both-endian (LE first)
	drDateOff   = 18 // 7-byte recording date
	drNameLenOf = 32 // length of the name field
)

func normalizeTimestamps(path string, stamp time.Time) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	stamp = stamp.UTC()
	// Directory extents to patch, deduped — the PVD and SVD path tables can
	// reference shared extents (go-diskfs points Joliet subdirectory records
	// at the ISO9660 extents).
	dirExtents := map[uint32]bool{}

	for _, sector := range []int64{pvdSector, svdSector} {
		buf := make([]byte, isoBlocksize)
		if _, err := f.ReadAt(buf, sector*isoBlocksize); err != nil {
			return fmt.Errorf("read volume descriptor sector %d: %w", sector, err)
		}
		// Type 1 = PVD, 2 = SVD; anything else (terminator) is left alone.
		if (buf[0] != 1 && buf[0] != 2) || string(buf[1:6]) != "CD001" {
			continue
		}
		for _, off := range []int{vdCreationOff, vdModifiedOff, vdExpirationOff, vdEffectiveOff} {
			copy(buf[off:off+vdDecDatetimeSize], decDatetime(stamp))
		}
		copy(buf[vdRootRecordOff+drDateOff:], binDatetime(stamp))
		if _, err := f.WriteAt(buf, sector*isoBlocksize); err != nil {
			return fmt.Errorf("write volume descriptor sector %d: %w", sector, err)
		}

		// Root extent (the path table lists it too, but cheap to seed).
		dirExtents[binary.LittleEndian.Uint32(buf[vdRootRecordOff+drExtentOff:])] = true
		// Every directory in this hierarchy, from the L path table.
		ptSize := binary.LittleEndian.Uint32(buf[vdPathTableSize:])
		ptLoc := binary.LittleEndian.Uint32(buf[vdPathTableLOff:])
		if err := collectPathTableDirs(f, ptLoc, ptSize, dirExtents); err != nil {
			return fmt.Errorf("path table (sector %d): %w", sector, err)
		}
	}

	for extent := range dirExtents {
		if err := normalizeDirExtent(f, extent, stamp); err != nil {
			return fmt.Errorf("directory extent %d: %w", extent, err)
		}
	}
	return nil
}

// collectPathTableDirs adds every directory extent listed in the L path table
// at ptLoc to out. Path table entry (ECMA-119 §9.4): name length (1), extended
// attribute length (1), extent (4, LE in the L table), parent number (2),
// name (padded to even length).
func collectPathTableDirs(f *os.File, ptLoc, ptSize uint32, out map[uint32]bool) error {
	buf := make([]byte, ptSize)
	if _, err := f.ReadAt(buf, int64(ptLoc)*isoBlocksize); err != nil {
		return err
	}
	for off := 0; off+8 <= len(buf); {
		nameLen := int(buf[off])
		if nameLen == 0 {
			break // zero-padded tail
		}
		out[binary.LittleEndian.Uint32(buf[off+2:])] = true
		off += 8 + nameLen + nameLen%2
	}
	return nil
}

// normalizeDirExtent rewrites the recording date and any Rock Ridge TF
// timestamps of every record in the directory at extent. The directory's own
// size comes from its leading "." record. Records never cross sector
// boundaries (a zero length byte pads to the next sector).
func normalizeDirExtent(f *os.File, extent uint32, stamp time.Time) error {
	head := make([]byte, 34)
	if _, err := f.ReadAt(head, int64(extent)*isoBlocksize); err != nil {
		return fmt.Errorf("read head: %w", err)
	}
	size := binary.LittleEndian.Uint32(head[drSizeOff:])
	if head[0] == 0 || size == 0 || size > 1<<24 {
		return fmt.Errorf("implausible directory (first record len %d, size %d)", head[0], size)
	}
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, int64(extent)*isoBlocksize); err != nil {
		return fmt.Errorf("read extent: %w", err)
	}

	for off := 0; off < len(buf); {
		recLen := int(buf[off])
		if recLen == 0 { // sector padding — jump to the next sector boundary
			off = (off/isoBlocksize + 1) * isoBlocksize
			continue
		}
		rec := buf[off : off+recLen]
		copy(rec[drDateOff:], binDatetime(stamp))
		normalizeSystemUse(rec, stamp)
		off += recLen
	}
	if _, err := f.WriteAt(buf, int64(extent)*isoBlocksize); err != nil {
		return fmt.Errorf("write extent: %w", err)
	}
	return nil
}

// normalizeSystemUse rewrites the timestamps of any Rock Ridge TF entry in a
// directory record's system-use area (SUSP). TF (RRIP §4.1.6): "TF" len ver
// flags, then one timestamp per set flag bit 0..6 — 17-byte dec-datetime form
// when bit 7 (LONG_FORM) is set, 7-byte binary form otherwise.
func normalizeSystemUse(rec []byte, stamp time.Time) {
	nameLen := int(rec[drNameLenOf])
	su := 33 + nameLen
	if nameLen%2 == 0 {
		su++ // pad byte keeps the system-use area at an even offset
	}
	for su+4 <= len(rec) {
		entryLen := int(rec[su+2])
		if entryLen < 4 || su+entryLen > len(rec) {
			return // padding / malformed — stop scanning this record
		}
		if rec[su] == 'T' && rec[su+1] == 'F' {
			flags := rec[su+4]
			stampLen := 7
			var encoded []byte
			if flags&0x80 != 0 {
				stampLen = vdDecDatetimeSize
				encoded = decDatetime(stamp)
			} else {
				encoded = binDatetime(stamp)
			}
			pos := su + 5
			for bit := 0; bit < 7; bit++ {
				if flags&(1<<bit) == 0 {
					continue
				}
				if pos+stampLen > su+entryLen {
					break
				}
				copy(rec[pos:pos+stampLen], encoded)
				pos += stampLen
			}
		}
		su += entryLen
	}
}

// decDatetime encodes the 17-byte ASCII dec-datetime (ECMA-119 §8.4.26.1):
// "YYYYMMDDHHMMSScc" + a GMT-offset byte (0 = UTC).
func decDatetime(t time.Time) []byte {
	s := fmt.Sprintf("%04d%02d%02d%02d%02d%02d%02d",
		t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0)
	return append([]byte(s), 0)
}

// binDatetime encodes the 7-byte binary datetime (ECMA-119 §9.1.5): years
// since 1900, month, day, hour, minute, second, GMT offset (0 = UTC).
func binDatetime(t time.Time) []byte {
	return []byte{
		byte(t.Year() - 1900), byte(t.Month()), byte(t.Day()),
		byte(t.Hour()), byte(t.Minute()), byte(t.Second()), 0,
	}
}
