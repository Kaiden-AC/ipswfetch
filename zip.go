// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 The ipswfetch contributors

package main

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
)

// This file implements a minimal remote ZIP reader in the spirit of
// partialZipBrowser (https://github.com/tihmstar/partialZipBrowser) and
// libfragmentzip: only the end-of-central-directory record and the central
// directory are fetched initially, then individual entries are pulled down
// with HTTP Range requests. It is an independent implementation in Go using
// only the standard library; no source from partialZipBrowser or
// libfragmentzip is included. See NOTICE.

const (
	sigEndOfCentralDir      = 0x06054b50
	sigZip64EndOfCentralDir = 0x06064b50
	sigZip64EOCDLocator     = 0x07064b50
	sigCentralDirEntry      = 0x02014b50
	sigLocalFileHeader      = 0x04034b50

	zip64ExtraField = 0x0001

	methodStore   = 0
	methodDeflate = 8

	eocdSearchSize = 1 << 17 // 128 KiB; EOCD + max comment is ~64 KiB
)

// ZipEntry describes a single file inside the remote archive.
type ZipEntry struct {
	Name              string
	Method            uint16
	CRC32             uint32
	CompressedSize    uint64
	UncompressedSize  uint64
	LocalHeaderOffset uint64
}

type RemoteZip struct {
	url     string
	client  *http.Client
	size    int64
	Entries []ZipEntry
}

func httpRange(client *http.Client, url string, start, length int64) (io.ReadCloser, error) {
	if length <= 0 {
		length = 1
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+length-1))
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("range request failed: %w", err)
	}
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("range request bytes=%d-%d failed with HTTP %d", start, start+length-1, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusOK {
		// Server ignored the Range header; we'd have to swallow the whole body.
		resp.Body.Close()
		return nil, fmt.Errorf("server does not support HTTP Range requests (refusing to download the whole IPSW)")
	}
	return resp.Body, nil
}

func fetchRange(client *http.Client, url string, start, length int64) ([]byte, error) {
	body, err := httpRange(client, url, start, length)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	buf := make([]byte, 0, length)
	w := bytes.NewBuffer(buf)
	lr := &limitedReader{r: body, n: length}
	if _, err := w.ReadFrom(lr); err != nil {
		return nil, err
	}
	return w.Bytes(), nil
}

type limitedReader struct {
	r io.Reader
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

// detectSize figures out the total size of the remote file using a
// bytes=-1 range probe and the Content-Range header.
func detectSize(client *http.Client, url string) (int64, error) {
	// First try HEAD.
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err == nil {
		req.Header.Set("User-Agent", userAgent)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.ContentLength > 0 && resp.StatusCode < 400 {
				return resp.ContentLength, nil
			}
		}
	}

	// Fall back to a 1-byte range probe.
	req, err = http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", "bytes=-1")
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPartialContent {
		cr := resp.Header.Get("Content-Range") // "bytes 123-456/789"
		if i := strings.LastIndex(cr, "/"); i != -1 {
			var total int64
			if _, err := fmt.Sscanf(cr[i+1:], "%d", &total); err == nil && total > 0 {
				return total, nil
			}
		}
		return 0, fmt.Errorf("could not parse Content-Range header %q", cr)
	}
	if resp.StatusCode == http.StatusOK {
		if resp.ContentLength > 0 {
			// The server sent the whole file; don't consume the body here.
			// We can't do partial reads, so bail out early.
			return 0, fmt.Errorf("server does not support HTTP Range requests")
		}
		return 0, fmt.Errorf("server does not support HTTP Range requests")
	}
	return 0, fmt.Errorf("range probe failed with HTTP %d", resp.StatusCode)
}

// OpenRemoteZip fetches the end-of-central-directory and central directory
// of the given remote ZIP file and returns its entries.
func OpenRemoteZip(url string) (*RemoteZip, error) {
	client := &http.Client{}

	size, err := detectSize(client, url)
	if err != nil {
		return nil, fmt.Errorf("could not determine size of remote file: %w", err)
	}

	tailLen := int64(eocdSearchSize)
	if size < tailLen {
		tailLen = size
	}
	tail, err := fetchRange(client, url, size-tailLen, tailLen)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch end of archive: %w", err)
	}

	eocd := findEOCD(tail)
	if eocd < 0 {
		return nil, fmt.Errorf("end of central directory not found; is this a ZIP file?")
	}

	cdSize := uint64(binary.LittleEndian.Uint32(tail[eocd+12:]))
	cdOffset := uint64(binary.LittleEndian.Uint32(tail[eocd+16:]))
	entriesOnDisk := uint64(binary.LittleEndian.Uint16(tail[eocd+8:]))
	totalEntries := uint64(binary.LittleEndian.Uint16(tail[eocd+10:]))
	_ = entriesOnDisk

	// ZIP64 handling: if any of the 32-bit fields are saturated, parse the
	// ZIP64 EOCD locator + record instead.
	if cdSize == 0xFFFFFFFF || cdOffset == 0xFFFFFFFF || totalEntries == 0xFFFF {
		loc := findSignature(tail, sigZip64EOCDLocator)
		if loc < 0 {
			return nil, fmt.Errorf("ZIP64 end of central directory locator not found")
		}
		zip64Offset := binary.LittleEndian.Uint64(tail[loc+8:])
		z64, err := fetchRange(client, url, int64(zip64Offset), 56)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch ZIP64 end of central directory: %w", err)
		}
		if binary.LittleEndian.Uint32(z64) != sigZip64EndOfCentralDir {
			return nil, fmt.Errorf("invalid ZIP64 end of central directory signature")
		}
		totalEntries = binary.LittleEndian.Uint64(z64[32:])
		cdSize = binary.LittleEndian.Uint64(z64[40:])
		cdOffset = binary.LittleEndian.Uint64(z64[48:])
	}

	cd, err := fetchRange(client, url, int64(cdOffset), int64(cdSize))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch central directory: %w", err)
	}

	entries, err := parseCentralDirectory(cd, totalEntries)
	if err != nil {
		return nil, err
	}

	return &RemoteZip{url: url, client: client, size: size, Entries: entries}, nil
}

func findSignature(buf []byte, sig uint32) int {
	for i := len(buf) - 4; i >= 0; i-- {
		if binary.LittleEndian.Uint32(buf[i:]) == sig {
			return i
		}
	}
	return -1
}

func findEOCD(buf []byte) int {
	for i := len(buf) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(buf[i:]) != sigEndOfCentralDir {
			continue
		}
		commentLen := int(binary.LittleEndian.Uint16(buf[i+20:]))
		if i+22+commentLen == len(buf) {
			return i
		}
	}
	return -1
}

func parseCentralDirectory(cd []byte, expected uint64) ([]ZipEntry, error) {
	var entries []ZipEntry
	off := 0
	for off+46 <= len(cd) {
		sig := binary.LittleEndian.Uint32(cd[off:])
		if sig != sigCentralDirEntry {
			return nil, fmt.Errorf("invalid central directory entry signature 0x%08x at offset %d", sig, off)
		}

		method := binary.LittleEndian.Uint16(cd[off+10:])
		crc := binary.LittleEndian.Uint32(cd[off+16:])
		compSize := uint64(binary.LittleEndian.Uint32(cd[off+20:]))
		uncompSize := uint64(binary.LittleEndian.Uint32(cd[off+24:]))
		nameLen := int(binary.LittleEndian.Uint16(cd[off+28:]))
		extraLen := int(binary.LittleEndian.Uint16(cd[off+30:]))
		commentLen := int(binary.LittleEndian.Uint16(cd[off+32:]))
		localOffset := uint64(binary.LittleEndian.Uint32(cd[off+42:]))

		nameStart := off + 46
		extraStart := nameStart + nameLen
		nextEntry := extraStart + extraLen + commentLen
		if nextEntry > len(cd) {
			return nil, fmt.Errorf("truncated central directory entry at offset %d", off)
		}

		// ZIP64 extra field fills in any fields that were 0xFFFFFFFF, in order:
		// uncompressed size, compressed size, local header offset, disk start.
		extra := cd[extraStart : extraStart+extraLen]
		for eoff := 0; eoff+4 <= len(extra); {
			headerID := binary.LittleEndian.Uint16(extra[eoff:])
			dataSize := int(binary.LittleEndian.Uint16(extra[eoff+2:]))
			dataStart := eoff + 4
			if dataStart+dataSize > len(extra) {
				break
			}
			if headerID == zip64ExtraField {
				p := dataStart
				read64 := func() uint64 {
					v := binary.LittleEndian.Uint64(extra[p:])
					p += 8
					return v
				}
				if uncompSize == 0xFFFFFFFF {
					uncompSize = read64()
				}
				if compSize == 0xFFFFFFFF {
					compSize = read64()
				}
				if localOffset == 0xFFFFFFFF {
					localOffset = read64()
				}
				break
			}
			eoff = dataStart + dataSize
		}

		entries = append(entries, ZipEntry{
			Name:              string(cd[nameStart:extraStart]),
			Method:            method,
			CRC32:             crc,
			CompressedSize:    compSize,
			UncompressedSize:  uncompSize,
			LocalHeaderOffset: localOffset,
		})
		off = nextEntry
	}
	return entries, nil
}

// Download streams the given entry to dst, verifying its CRC32.
func (z *RemoteZip) Download(entry ZipEntry, dst io.Writer, progress func(done, total uint64)) error {
	// Local file header: fixed 30 bytes, then variable name/extra.
	head, err := fetchRange(z.client, z.url, int64(entry.LocalHeaderOffset), 30)
	if err != nil {
		return fmt.Errorf("failed to fetch local file header for %s: %w", entry.Name, err)
	}
	if binary.LittleEndian.Uint32(head) != sigLocalFileHeader {
		return fmt.Errorf("invalid local file header signature for %s", entry.Name)
	}
	lNameLen := uint64(binary.LittleEndian.Uint16(head[26:]))
	lExtraLen := uint64(binary.LittleEndian.Uint16(head[28:]))
	dataOffset := entry.LocalHeaderOffset + 30 + lNameLen + lExtraLen

	body, err := httpRange(z.client, z.url, int64(dataOffset), int64(entry.CompressedSize))
	if err != nil {
		return fmt.Errorf("failed to fetch data for %s: %w", entry.Name, err)
	}
	defer body.Close()

	pr := &progressReader{r: body, total: entry.CompressedSize, progress: progress}

	var src io.Reader = pr
	if entry.Method == methodDeflate {
		src = flate.NewReader(pr)
	} else if entry.Method != methodStore {
		return fmt.Errorf("unsupported compression method %d for %s", entry.Method, entry.Name)
	}

	h := crc32.NewIEEE()
	if _, err := io.Copy(io.MultiWriter(dst, h), src); err != nil {
		return fmt.Errorf("failed to write %s: %w", entry.Name, err)
	}

	if got := h.Sum32(); got != entry.CRC32 {
		return fmt.Errorf("CRC32 mismatch for %s (got 0x%08x, want 0x%08x)", entry.Name, got, entry.CRC32)
	}
	return nil
}

type progressReader struct {
	r        io.Reader
	total    uint64
	done     uint64
	progress func(done, total uint64)
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	p.done += uint64(n)
	if p.progress != nil && p.total > 0 {
		p.progress(p.done, p.total)
	}
	return n, err
}
