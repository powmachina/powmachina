// Package geoip finds the country of an IP address, for the SharePool page's map of where the
// shares come from. It reads DB-IP's "IP to Country Lite" database as CSV (start,end,country per
// line; CC BY 4.0, https://db-ip.com — a page that shows its result says so). Nothing here is part
// of consensus, and nothing keeps an address: a lookup returns a country and that is all.
package geoip

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"strings"
)

type range4 struct {
	start, end uint32
	cc         [2]byte
}

type range6 struct {
	start, end [16]byte
	cc         [2]byte
}

// DB is the database in memory: the ranges of each family, sorted by their start.
type DB struct {
	v4 []range4
	v6 []range6
}

// Load reads the CSV, plain or gzipped (by the .gz suffix).
func Load(path string) (*DB, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		z, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer z.Close()
		r = z
	}
	return Read(r)
}

// Read parses the CSV from r.
func Read(r io.Reader) (*DB, error) {
	db := &DB{}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		parts := strings.Split(strings.TrimSpace(sc.Text()), ",")
		if len(parts) == 1 && parts[0] == "" {
			continue
		}
		if len(parts) != 3 || len(parts[2]) != 2 {
			return nil, fmt.Errorf("geoip: line %d: want start,end,country", line)
		}
		a, err1 := netip.ParseAddr(strings.Trim(parts[0], `"`))
		b, err2 := netip.ParseAddr(strings.Trim(parts[1], `"`))
		if err1 != nil || err2 != nil || a.Is4() != b.Is4() {
			return nil, fmt.Errorf("geoip: line %d: not a range", line)
		}
		cc := [2]byte{parts[2][0], parts[2][1]}
		if a.Is4() {
			db.v4 = append(db.v4, range4{u32(a), u32(b), cc})
		} else {
			db.v6 = append(db.v6, range6{a.As16(), b.As16(), cc})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(db.v4)+len(db.v6) == 0 {
		return nil, errors.New("geoip: the database is empty")
	}
	sort.Slice(db.v4, func(i, j int) bool { return db.v4[i].start < db.v4[j].start })
	sort.Slice(db.v6, func(i, j int) bool { return bytes.Compare(db.v6[i].start[:], db.v6[j].start[:]) < 0 })
	return db, nil
}

func u32(a netip.Addr) uint32 { b := a.As4(); return binary.BigEndian.Uint32(b[:]) }

// Country is the ISO 3166-1 alpha-2 code of the address's country; ok is false when the database
// does not know it ("ZZ" is DB-IP's unknown, and is not a country either).
func (db *DB) Country(a netip.Addr) (string, bool) {
	if db == nil || !a.IsValid() {
		return "", false
	}
	a = a.Unmap()
	var cc [2]byte
	if a.Is4() {
		x := u32(a)
		i := sort.Search(len(db.v4), func(i int) bool { return db.v4[i].start > x }) - 1
		if i < 0 || x > db.v4[i].end {
			return "", false
		}
		cc = db.v4[i].cc
	} else {
		x := a.As16()
		i := sort.Search(len(db.v6), func(i int) bool { return bytes.Compare(db.v6[i].start[:], x[:]) > 0 }) - 1
		if i < 0 || bytes.Compare(x[:], db.v6[i].end[:]) > 0 {
			return "", false
		}
		cc = db.v6[i].cc
	}
	if cc == [2]byte{'Z', 'Z'} {
		return "", false
	}
	return string(cc[:]), true
}

// Len is how many ranges the database holds.
func (db *DB) Len() int { return len(db.v4) + len(db.v6) }
