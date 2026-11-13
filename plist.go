// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 The ipswfetch contributors

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Minimal XML plist decoder, sufficient for Apple's BuildManifest.plist.
// Dictionaries decode to map[string]interface{}, arrays to []interface{}.

func parsePlistXML(data []byte) (interface{}, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var root interface{}
	found := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid plist XML: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local != "plist" {
			v, err := parsePlistValue(dec, se)
			if err != nil {
				return nil, err
			}
			root = v
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("no root element found in plist")
	}
	return root, nil
}

func plistCharData(dec *xml.Decoder, endName string) (string, error) {
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			b.Write([]byte(t))
		case xml.EndElement:
			if t.Name.Local == endName {
				return b.String(), nil
			}
		case xml.StartElement:
			return "", fmt.Errorf("unexpected <%s> inside <%s>", t.Name.Local, endName)
		}
	}
}

func nextPlistElement(dec *xml.Decoder) (xml.StartElement, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se, nil
		}
	}
}

func parsePlistValue(dec *xml.Decoder, start xml.StartElement) (interface{}, error) {
	switch start.Name.Local {
	case "dict":
		m := make(map[string]interface{})
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, fmt.Errorf("unterminated <dict>: %w", err)
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local != "key" {
					return nil, fmt.Errorf("expected <key> in <dict>, got <%s>", t.Name.Local)
				}
				key, err := plistCharData(dec, "key")
				if err != nil {
					return nil, err
				}
				valStart, err := nextPlistElement(dec)
				if err != nil {
					return nil, err
				}
				val, err := parsePlistValue(dec, valStart)
				if err != nil {
					return nil, err
				}
				m[key] = val
			case xml.EndElement:
				if t.Name.Local == "dict" {
					return m, nil
				}
			}
		}
	case "array":
		var a []interface{}
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, fmt.Errorf("unterminated <array>: %w", err)
			}
			switch t := tok.(type) {
			case xml.StartElement:
				v, err := parsePlistValue(dec, t)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			case xml.EndElement:
				if t.Name.Local == "array" {
					return a, nil
				}
			}
		}
	case "string", "key":
		return plistCharData(dec, start.Name.Local)
	case "integer":
		s, err := plistCharData(dec, "integer")
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid plist integer %q", s)
		}
		return n, nil
	case "real":
		s, err := plistCharData(dec, "real")
		if err != nil {
			return nil, err
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid plist real %q", s)
		}
		return f, nil
	case "true":
		if _, err := plistCharData(dec, "true"); err != nil {
			return nil, err
		}
		return true, nil
	case "false":
		if _, err := plistCharData(dec, "false"); err != nil {
			return nil, err
		}
		return false, nil
	case "date":
		return plistCharData(dec, "date")
	case "data":
		s, err := plistCharData(dec, "data")
		if err != nil {
			return nil, err
		}
		b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
		if err != nil {
			return nil, fmt.Errorf("invalid plist data: %w", err)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("unsupported plist element <%s>", start.Name.Local)
	}
}

func plistDict(v interface{}) map[string]interface{} {
	m, _ := v.(map[string]interface{})
	return m
}

func plistArray(v interface{}) []interface{} {
	a, _ := v.([]interface{})
	return a
}

func plistString(v interface{}) string {
	s, _ := v.(string)
	return s
}

// dictGetCI does a case-insensitive dictionary lookup.
func dictGetCI(m map[string]interface{}, key string) interface{} {
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return nil
}

// selectIdentity picks the build identity for the given boardconfig
// (e.g. "d101ap"). Identities with a "Variant" containing "Upgrade" are
// preferred, as those are the normal restore path.
func selectIdentity(identities []interface{}, boardconfig string) map[string]interface{} {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		// Normalize "d101ap" -> "d101" so "D101AP" matches either form.
		return strings.TrimSuffix(s, "ap")
	}
	want := norm(boardconfig)

	var upgrade, other map[string]interface{}
	for _, iv := range identities {
		ident := plistDict(iv)
		if ident == nil {
			continue
		}
		info := plistDict(dictGetCI(ident, "Info"))
		if info == nil {
			continue
		}
		class := plistString(dictGetCI(info, "DeviceClass"))
		if norm(class) != want {
			continue
		}
		variant := plistString(dictGetCI(info, "Variant"))
		if other == nil {
			other = ident
		}
		if strings.Contains(strings.ToLower(variant), "upgrade") {
			upgrade = ident
		}
	}
	if upgrade != nil {
		return upgrade
	}
	return other
}
