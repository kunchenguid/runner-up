package agent

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"unicode/utf16"
)

// seedISO writes cloud-init's NoCloud seed for a Hyper-V place: a CD image
// labelled cidata holding files, in the root folder only. Windows has no
// program to make one, so runner-up writes it: ISO 9660 with Joliet names,
// since cloud-init's names (user-data, meta-data) are not plain ISO 9660
// ones, and Linux reads the Joliet names when both are there.
func seedISO(files map[string][]byte) ([]byte, error) {
	const sector = 2048
	names := make([]string, 0, len(files))
	for name := range files {
		if name == "" || len(name) > 60 || strings.ContainsAny(name, "/\\;*:?\"<>|") {
			return nil, fmt.Errorf("seed file name %q is not allowed", name)
		}
		names = append(names, name)
	}
	slices.Sort(names)

	// Sectors 0-15 are the system area; then the primary and Joliet
	// descriptors, the end of descriptors, the four path tables, the two
	// root folders and the files.
	const (
		pvdAt, svdAt, endAt          = 16, 17, 18
		lPathAt, mPathAt             = 19, 20
		lJolietAt, mJolietAt         = 21, 22
		rootAt, jolietRootAt, fileAt = 23, 24, 25
	)
	at := make(map[string]uint32, len(names))
	next := uint32(fileAt)
	for _, name := range names {
		at[name] = next
		next += uint32((len(files[name]) + sector - 1) / sector)
	}
	total := next

	// Plain ISO 9660 names: upper-case letters, digits and _, at most eight
	// and three, unique among themselves.
	plain := make(map[string]string, len(names))
	used := map[string]bool{}
	for i, name := range names {
		base, ext, _ := strings.Cut(strings.ToUpper(name), ".")
		clean := func(s string, n int) string {
			var b strings.Builder
			for _, r := range s {
				if b.Len() == n {
					break
				}
				if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
					b.WriteRune(r)
				} else if r != '-' {
					b.WriteRune('_')
				}
			}
			return b.String()
		}
		p := clean(base, 8) + "." + clean(ext, 3) + ";1"
		if used[p] || strings.HasPrefix(p, ".") {
			p = fmt.Sprintf("FILE%04d.;1", i)
		}
		used[p] = true
		plain[name] = p
	}

	img := make([]byte, int(total)*sector)
	put := func(lba uint32, b []byte) { copy(img[int(lba)*sector:], b) }

	// record is one folder entry.
	record := func(id []byte, extent uint32, size uint32, dir bool) []byte {
		n := 33 + len(id)
		if n%2 == 1 {
			n++
		}
		r := make([]byte, n)
		r[0] = byte(n)
		both32(r[2:], extent)
		both32(r[10:], size)
		// The time is 1 January 2000, 00:00 UTC: an image's bytes depend
		// only on its files.
		copy(r[18:25], []byte{100, 1, 1, 0, 0, 0, 0})
		if dir {
			r[25] = 2
		}
		both16(r[28:], 1)
		r[32] = byte(len(id))
		copy(r[33:], id)
		return r
	}
	folder := func(self uint32, name func(string) []byte) []byte {
		var b bytes.Buffer
		b.Write(record([]byte{0}, self, sector, true))
		b.Write(record([]byte{1}, self, sector, true))
		for _, n := range names {
			b.Write(record(name(n), at[n], uint32(len(files[n])), false))
		}
		return b.Bytes()
	}
	root := folder(rootAt, func(n string) []byte { return []byte(plain[n]) })
	jroot := folder(jolietRootAt, func(n string) []byte { return ucs2(n + ";1") })
	if len(root) > sector || len(jroot) > sector {
		return nil, fmt.Errorf("too many seed files")
	}
	put(rootAt, root)
	put(jolietRootAt, jroot)
	for _, n := range names {
		put(at[n], files[n])
	}

	// One path table entry each: the root folder.
	pathTable := func(extent uint32, big bool) []byte {
		t := make([]byte, 10)
		t[0] = 1
		if big {
			binary.BigEndian.PutUint32(t[2:], extent)
			binary.BigEndian.PutUint16(t[6:], 1)
		} else {
			binary.LittleEndian.PutUint32(t[2:], extent)
			binary.LittleEndian.PutUint16(t[6:], 1)
		}
		return t
	}
	put(lPathAt, pathTable(rootAt, false))
	put(mPathAt, pathTable(rootAt, true))
	put(lJolietAt, pathTable(jolietRootAt, false))
	put(mJolietAt, pathTable(jolietRootAt, true))

	descriptor := func(kind byte, text func(string, int) []byte, rootRec []byte, lPath, mPath uint32) []byte {
		d := make([]byte, sector)
		d[0] = kind
		copy(d[1:6], "CD001")
		d[6] = 1
		copy(d[8:40], text("", 32))
		copy(d[40:72], text("cidata", 32))
		both32(d[80:], total)
		both16(d[120:], 1)
		both16(d[124:], 1)
		both16(d[128:], sector)
		both32(d[132:], 10)
		binary.LittleEndian.PutUint32(d[140:], lPath)
		binary.BigEndian.PutUint32(d[148:], mPath)
		copy(d[156:190], rootRec)
		for _, f := range [][2]int{{190, 128}, {318, 128}, {446, 128}, {574, 128}, {702, 37}, {739, 37}, {776, 37}} {
			copy(d[f[0]:f[0]+f[1]], text("", f[1]))
		}
		// No dates.
		for _, off := range []int{813, 830, 847, 864} {
			copy(d[off:off+16], "0000000000000000")
		}
		d[881] = 1
		return d
	}
	ascii := func(s string, n int) []byte { return []byte(fmt.Sprintf("%-*s", n, s)) }
	joliet := func(s string, n int) []byte {
		b := ucs2(s)
		for len(b)+2 <= n {
			b = append(b, 0, ' ')
		}
		return b[:n-n%2]
	}
	pvd := descriptor(1, ascii, record([]byte{0}, rootAt, sector, true), lPathAt, mPathAt)
	svd := descriptor(2, joliet, record([]byte{0}, jolietRootAt, sector, true), lJolietAt, mJolietAt)
	// UCS-2 level 3.
	copy(svd[88:91], "%/E")
	put(pvdAt, pvd)
	put(svdAt, svd)
	end := make([]byte, 7)
	end[0] = 255
	copy(end[1:6], "CD001")
	end[6] = 1
	put(endAt, end)
	return img, nil
}

func ucs2(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.BigEndian.PutUint16(b[2*i:], c)
	}
	return b
}

// both16 and both32 write v little-endian then big-endian, as ISO 9660 keeps
// its numbers.
func both16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
}

func both32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
}
