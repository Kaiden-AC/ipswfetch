// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 The ipswfetch contributors
//
// Portions of this file are derived from wikiproxy-go
// (https://github.com/LukeZGD/wikiproxy-go), Copyright (c) 2026 LukeZGD,
// used under the MIT License. See NOTICE and LICENSES/wikiproxy-go-LICENSE.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const (
	wikiAPI    = "https://theapplewiki.com/api.php"
	userAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	httpStatus = 200
)

type wikiSearchResponse struct {
	Query struct {
		SearchInfo struct {
			TotalHits int `json:"totalhits"`
		} `json:"searchinfo"`
		Search []struct {
			Title string `json:"title"`
		} `json:"search"`
	} `json:"query"`
}

type wikiParseResponse struct {
	Parse struct {
		Wikitext string `json:"wikitext"`
	} `json:"parse"`
}

// FirmwareInfo holds the interesting fields from a keys page on The Apple Wiki.
type FirmwareInfo struct {
	Device      string
	Version     string
	Build       string
	Codename    string
	DownloadURL string
	// PageData is the raw parsed key/value map from the keys page (e.g.
	// "DeviceTree" -> "DeviceTree.n82ap.img3"). Used to resolve component
	// paths for firmwares too old to carry a BuildManifest.plist.
	PageData map[string]string
}

func wikiGet(rawURL string, v interface{}) error {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	// The Apple Wiki refuses to serve wikitext to non-browser agents.
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s failed: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != httpStatus {
		return fmt.Errorf("request to %s failed with HTTP %d: %s", rawURL, resp.StatusCode, resp.Status)
	}

	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("failed to decode wiki response: %w", err)
	}
	return nil
}

// fetchWikitext downloads the raw wikitext of the given page title.
func fetchWikitext(title string) (string, error) {
	parseURL := fmt.Sprintf(
		"%s?action=parse&prop=wikitext&page=%s&format=json&formatversion=2",
		wikiAPI, url.QueryEscape(title))

	var parseData wikiParseResponse
	if err := wikiGet(parseURL, &parseData); err != nil {
		return "", err
	}
	return parseData.Parse.Wikitext, nil
}

// parseKeyWikitext parses the {{keys ...}} template into a key/value map.
func parseKeyWikitext(data string) map[string]string {
	pageData := make(map[string]string)

	start := strings.Index(data, "{{")
	end := strings.Index(data, "}}")
	if start == -1 || end == -1 || end <= start {
		return pageData
	}

	for _, part := range strings.Split(data[start+2:end], "|") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(strings.ReplaceAll(kv[1], "\n", " "))
		val = strings.TrimSpace(strings.Join(strings.Fields(val), " "))
		if key != "" {
			pageData[key] = val
		}
	}
	return pageData
}

// extractURL pulls a bare http(s) URL out of a wiki value. Values are usually
// a plain URL but sometimes wrapped in [brackets] or piped link syntax.
func extractURL(v string) string {
	idx := strings.Index(v, "http")
	if idx == -1 {
		return ""
	}
	rest := v[idx:]
	end := len(rest)
	for _, sep := range []string{" ", "]", "|", ")", "<"} {
		if i := strings.Index(rest, sep); i != -1 && i < end {
			end = i
		}
	}
	return strings.TrimRight(rest[:end], ".,;:)")
}

var buildPattern = regexp.MustCompile(`^[0-9]+[A-Za-z][0-9A-Za-z]*$`)
var versionNumPattern = regexp.MustCompile(`\d+(\.\d+)*`)
var buildLikePattern = regexp.MustCompile(`(?i)\b[0-9]+[a-z][0-9a-z]*\b`)
var boardConfigPattern = regexp.MustCompile(`(?i)^[A-Za-z0-9]{2,10}AP$`)

// LookupBoardConfig finds candidate boardconfigs for a device (e.g.
// "iPhone9,3" -> ["D101AP"]) from the identifiers section of its The Apple
// Wiki article. Device articles come in a few shapes: tables (cells separated
// by "|", rows by "|-") and bullet lists (one variant per line), so both are
// scanned. Returns all distinct candidates found.
func LookupBoardConfig(device string) ([]string, error) {
	searchURL := fmt.Sprintf(
		"%s?action=query&list=search&srsearch=%s&srlimit=5&format=json&srnamespace=0",
		wikiAPI, url.QueryEscape(fmt.Sprintf("intitle:%s", device)))

	var searchData wikiSearchResponse
	if err := wikiGet(searchURL, &searchData); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var candidates []string
	add := func(s string) {
		key := strings.ToLower(s)
		if s != "" && !seen[key] {
			seen[key] = true
			candidates = append(candidates, s)
		}
	}

	for _, hit := range searchData.Query.Search {
		wikitext, err := fetchWikitext(hit.Title)
		if err != nil {
			continue
		}

		// Strategy 1: MediaWiki table rows ("|-") containing the device;
		// the boardconfig is a later cell in the same row.
		for _, row := range strings.Split(wikitext, "|-") {
			cells := strings.Split(row, "|")
			devIdx := -1
			for i, cell := range cells {
				c := strings.TrimSpace(strings.Trim(cell, "{|}"))
				if strings.EqualFold(c, device) {
					devIdx = i
					break
				}
			}
			if devIdx == -1 {
				continue
			}
			// The boardconfig cell may list several boards, e.g.
			// "J71sAP ([[S8000]])<br />J71tAP ([[S8003]])" — collect all
			// AP-suffixed tokens on the row's cells after the device cell.
			for _, bcCell := range cells[devIdx+1:] {
				for _, tok := range alphanumericTokens(bcCell) {
					if boardConfigPattern.MatchString(tok) {
						add(tok)
					}
				}
			}
		}

		// Strategy 2: any line containing the device token; the boardconfig
		// is any AP-suffixed alphanumeric token on the same line.
		for _, line := range strings.Split(wikitext, "\n") {
			if !strings.Contains(strings.ToLower(line), strings.ToLower(device)) {
				continue
			}
			for _, tok := range alphanumericTokens(line) {
				if boardConfigPattern.MatchString(tok) {
					add(tok)
				}
			}
		}
	}
	return candidates, nil
}

func alphanumericTokens(s string) []string {
	var tokens []string
	start := -1
	for i, r := range s {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if isAlnum {
			if start == -1 {
				start = i
			}
		} else if start != -1 {
			tokens = append(tokens, s[start:i])
			start = -1
		}
	}
	if start != -1 {
		tokens = append(tokens, s[start:])
	}
	return tokens
}

// LookupFirmware searches The Apple Wiki keys namespace for a page matching
// the given device and version. The version may be a marketing version
// ("15.4") or a build number ("19E241"). It returns the best exact match.
func LookupFirmware(device, version string) (*FirmwareInfo, error) {
	searchURL := fmt.Sprintf(
		"%s?action=query&list=search&srsearch=%s&srlimit=20&format=json&srnamespace=2304",
		wikiAPI, url.QueryEscape(fmt.Sprintf("intitle:%s %s", device, version)))

	var searchData wikiSearchResponse
	if err := wikiGet(searchURL, &searchData); err != nil {
		return nil, err
	}
	if len(searchData.Query.Search) == 0 {
		return nil, fmt.Errorf("no keys page found on The Apple Wiki for device %q version %q", device, version)
	}

	wantBuild := buildPattern.MatchString(version)

	// Prefer a page whose Version (or Build) exactly matches the query.
	for _, hit := range searchData.Query.Search {
		wikitext, err := fetchWikitext(hit.Title)
		if err != nil {
			continue
		}
		page := parseKeyWikitext(wikitext)

		ver, build := page["Version"], page["Build"]

		// Version fields often contain wiki markup, e.g.
		// "15.4 [[Release Candidate|RC]] and 15.4"; extract the bare number.
		pageVersion := versionNumPattern.FindString(ver)
		queryVersion := versionNumPattern.FindString(version)
		pageBuild := buildLikePattern.FindString(build)

		info := &FirmwareInfo{
			Device:      page["Device"],
			Version:     pageVersion,
			Build:       pageBuild,
			Codename:    page["Codename"],
			DownloadURL: extractURL(page["DownloadURL"]),
			PageData:    page,
		}

		matched := false
		if wantBuild {
			matched = strings.EqualFold(pageBuild, version)
		} else {
			matched = pageVersion != "" && pageVersion == queryVersion
		}
		if matched {
			if info.DownloadURL == "" {
				return nil, fmt.Errorf("wiki page %q has no DownloadURL (the IPSW may not be available for download)", hit.Title)
			}
			return info, nil
		}
	}

	// Never substitute a near-miss: only an exact Version/Build match is
	// acceptable. Otherwise report what we found.
	candidates := make([]string, 0, len(searchData.Query.Search))
	for _, hit := range searchData.Query.Search {
		candidates = append(candidates, hit.Title)
	}
	return nil, fmt.Errorf("no exact keys page for %q %q; candidates: %s",
		device, version, strings.Join(candidates, ", "))
}
