// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 The ipswfetch contributors

// ipswfetch downloads the iBSS, iBEC, DeviceTree and kernelcache out of the
// IPSW for any device and version, straight from Apple's CDN, without
// downloading the whole multi-gigabyte archive.
//
// Usage:
//
//	ipswfetch [flags] <device> <version> [output-dir]
//	ipswfetch -device iPhone9,3 -version 15.4 [-o output-dir]
//
// The version may be a marketing version ("15.4") or a build number ("19E241").
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// component pairs the display name of a requested image with the predicate
// used to find it in the IPSW file listing, plus the BuildManifest tag used
// to resolve it authoritatively.
type component struct {
	name  string
	tag   string
	match func(base string) bool
}

var defaultComponents = []component{
	{"iBSS", "iBSS", func(b string) bool {
		return strings.HasPrefix(strings.ToLower(b), "ibss.")
	}},
	{"iBEC", "iBEC", func(b string) bool {
		return strings.HasPrefix(strings.ToLower(b), "ibec.")
	}},
	{"DeviceTree", "DeviceTree", func(b string) bool {
		return strings.HasPrefix(strings.ToLower(b), "devicetree")
	}},
	{"kernelcache", "KernelCache", func(b string) bool {
		return strings.HasPrefix(strings.ToLower(b), "kernelcache")
	}},
}

// normalizeTag strips everything except ASCII alphanumerics and lowercases,
// so "Restore-Ram-Disk", "RestoreRamDisk" and "restoreramdisk" all compare
// equal.
func normalizeTag(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tagEquivalents returns the normalized keys that are considered the same
// tag (The Apple Wiki uses RootFS / SEPFirmware, BuildManifest uses OS / SEP).
func tagEquivalents(tag string) []string {
	norm := normalizeTag(tag)
	switch norm {
	case "rootfs", "os":
		return []string{"rootfs", "os"}
	case "sep", "sepfirmware":
		return []string{"sep", "sepfirmware"}
	default:
		return []string{norm}
	}
}

func containsNorm(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// findZipContaining returns the path of a zip entry whose base name contains
// the (already normalized) needle, preferring an exact match.
func findZipContaining(entries []ZipEntry, needle string) string {
	best := ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		eb := normalizeTag(filepath.Base(e.Name))
		if !strings.Contains(eb, needle) {
			continue
		}
		if eb == needle {
			return e.Name
		}
		if best == "" || len(e.Name) < len(best) {
			best = e.Name
		}
	}
	return best
}

// wikiComponentPaths resolves requested components from the keys page of The
// Apple Wiki, which lists the exact file for every encrypted component (and
// is the only machine-readable mapping for firmwares too old to carry a
// BuildManifest.plist). Unlisted (unencrypted) components are found later by
// filename matching.
func wikiComponentPaths(pageData map[string]string, entries []ZipEntry, comps []component) map[string]string {
	out := map[string]string{}
	for _, c := range comps {
		cands := tagEquivalents(c.tag)
		for k, v := range pageData {
			if !containsNorm(cands, normalizeTag(k)) {
				continue
			}
			nv := normalizeTag(v)
			if len(nv) < 4 {
				continue
			}
			if p := findZipContaining(entries, nv); p != "" {
				out[c.name] = p
			}
			break
		}
	}
	return out
}

var wikiMetaKeys = map[string]bool{
	"version": true, "build": true, "device": true, "codename": true,
	"baseband": true, "downloadurl": true, "releasetype": true, "branding": true,
}

// wikiResolvingTags lists the keys-page tags that map to an actual file in
// the archive (skipping IV/Key/KBAG fields and metadata like Version).
func wikiResolvingTags(pageData map[string]string, entries []ZipEntry) []string {
	if len(pageData) == 0 {
		return nil
	}
	var tags []string
	for k, v := range pageData {
		norm := normalizeTag(k)
		if wikiMetaKeys[norm] ||
			strings.HasSuffix(norm, "iv") ||
			strings.HasSuffix(norm, "key") ||
			strings.HasSuffix(norm, "kbag") {
			continue
		}
		nv := normalizeTag(v)
		if len(nv) < 4 {
			continue
		}
		if findZipContaining(entries, nv) == "" {
			continue
		}
		tags = append(tags, k)
	}
	sort.Strings(tags)
	return tags
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

var zipTagCanonical = map[string]string{
	"ibss": "iBSS", "ibec": "iBEC", "wtf": "WTF", "llb": "LLB",
	"iboot": "iBoot", "devicetree": "DeviceTree", "kernelcache": "KernelCache",
	"applelogo": "AppleLogo", "recoverymode": "RecoveryMode", "needservice": "NeedService",
	"glyphcharging": "GlyphCharging", "glyphplugin": "GlyphPlugin",
	"batterycharging": "BatteryCharging", "batteryfull": "BatteryFull", "batterylow": "BatteryLow",
}

var zipTagNoise = map[string]bool{
	"fseventsd": true, "the": true, "img": true, "manifest": false,
}

// deriveZipTags derives candidate image tags from the archive listing by
// looking at leading letters of each file name (e.g. "LLB.m68ap.RELEASE.img3"
// -> "LLB"). Used to offer a tag list when there is no BuildManifest.plist
// to enumerate from.
func deriveZipTags(entries []ZipEntry) []string {
	seen := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := filepath.Base(e.Name)
		i := 0
		for i < len(base) && isASCIILetter(base[i]) {
			i++
		}
		if i < 2 {
			continue
		}
		tok := base[:i]
		key := strings.ToLower(tok)
		if skip, noise := zipTagNoise[key]; noise && skip {
			continue
		}
		if _, ok := seen[key]; !ok {
			if c, ok := zipTagCanonical[key]; ok {
				seen[key] = c
			} else {
				seen[key] = tok
			}
		}
	}
	out := make([]string, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

var allFlashBoardRe = regexp.MustCompile(`(?i)all_flash\.([a-z0-9_]+)\.production`)
var dfuBoardRe = regexp.MustCompile(`(?i)/(?:iBSS|iBEC|WTF)\.([a-z0-9_]+)\.`)

// deriveZipBoards extracts distinct boardconfigs from all_flash directory
// names and DFU file names, for firmwares too old to declare boards in a
// BuildManifest (or a Restore.plist we can cheaply trust).
func deriveZipBoards(entries []ZipEntry) []string {
	set := map[string]bool{}
	for _, e := range entries {
		if m := allFlashBoardRe.FindStringSubmatch(e.Name); m != nil {
			set[strings.ToLower(m[1])] = true
		}
		if m := dfuBoardRe.FindStringSubmatch(e.Name); m != nil {
			set[strings.ToLower(m[1])] = true
		}
	}
	out := make([]string, 0, len(set))
	for b := range set {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// requestedComponents builds the component list from the -image flag. When
// images is empty the default set (iBSS, iBEC, DeviceTree, kernelcache) is
// returned.
func requestedComponents(images []string) []component {
	if len(images) == 0 {
		return defaultComponents
	}
	seen := map[string]bool{}
	var out []component
	for _, img := range images {
		img = strings.TrimSpace(img)
		norm := normalizeTag(img)
		if norm == "" || seen[norm] {
			continue
		}
		seen[norm] = true
		img := img // capture for the closure
		normCpy := norm
		out = append(out, component{
			name: img,
			tag:  img,
			match: func(base string) bool {
				return strings.Contains(normalizeTag(base), normCpy)
			},
		})
	}
	return out
}

// findManifestTag resolves a component tag to its file path inside the IPSW
// via a parsed BuildManifest's Manifest dictionary. Matching is
// case-insensitive and ignores non-alphanumerics. Returns the path.
func findManifestTag(manifest map[string]interface{}, tag string) (string, bool) {
	cands := tagEquivalents(tag)
	for k, v := range manifest {
		if !containsNorm(cands, normalizeTag(k)) {
			continue
		}
		entry := plistDict(v)
		if entry == nil {
			break
		}
		info := plistDict(dictGetCI(entry, "Info"))
		if info == nil {
			break
		}
		if p := plistString(dictGetCI(info, "Path")); p != "" {
			return p, true
		}
		return "", false
	}
	return "", false
}

// boardconfigMatch checks whether a path matches a boardconfig such as
// "d10" or "d101". It matches dot-separated segments like "d10ap" but avoids
// false positives such as "d10" matching "d101ap".
func boardconfigMatch(path, boardconfig string) bool {
	bc := strings.ToLower(boardconfig)
	for _, seg := range strings.Split(strings.ToLower(filepath.Base(path)), ".") {
		if seg == bc || seg == bc+"ap" {
			return true
		}
		if strings.HasPrefix(seg, bc) {
			rest := seg[len(bc):]
			hasDigit := false
			for _, r := range rest {
				if r >= '0' && r <= '9' {
					hasDigit = true
					break
				}
			}
			if !hasDigit {
				// e.g. seg == "d10ap" handled above; allow trailing "ap" only
				if rest == "" || rest == "ap" {
					return true
				}
			}
		}
	}
	// kernelcache files don't carry the boardconfig in their name.
	if strings.HasPrefix(strings.ToLower(filepath.Base(path)), "kernelcache") {
		return true
	}
	return false
}

type options struct {
	device        string
	version       string
	outputDir     string
	boardconfig   string
	listOnly      bool
	keepGoing     bool
	expectedBuild string
	images        []string
	tagsOnly      bool
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

func usage() {
	fmt.Fprintf(os.Stderr, `ipswfetch - download components out of an IPSW

Usage:
  ipswfetch [flags] <device> <version> [output-dir]

Arguments:
  device       e.g. iPhone9,3
  version      marketing version (15.4) or build number (19E241)
  output-dir   where to save the files (default: current folder)

Without -image, the iBSS, iBEC, DeviceTree and kernelcache are downloaded.
Use -tags to list the other images available in the IPSW, e.g. RestoreRamDisk.

Flags:
`)
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	deviceFlag := flag.String("device", "", "device identifier, e.g. iPhone9,3")
	versionFlag := flag.String("version", "", "marketing version (15.4) or build number (19E241)")
	outputFlag := flag.String("o", "", "output directory (default: current folder)")
	outputFlagLong := flag.String("output", "", "output directory (default: current folder)")
	boardconfigFlag := flag.String("boardconfig", "", "only download files matching this boardconfig (e.g. d10, d101)")
	var imageFlag stringList
	flag.Var(&imageFlag, "image", "download a specific image by BuildManifest tag instead of the default set (repeatable or comma-separated), e.g. -image RestoreRamDisk,LLB. Tags are case-insensitive and ignore punctuation (rootfs -> OS, sepfirmware -> SEP)")
	tagsFlag := flag.Bool("tags", false, "list the available image tags in the IPSW for the selected board and exit")
	listFlag := flag.Bool("list", false, "only list matching entries in the IPSW, don't download")
	keepGoingFlag := flag.Bool("keep-going", false, "continue downloading other components if one fails")
	flag.Usage = usage
	flag.Parse()

	opts := options{}
	pos := flag.Args()

	opts.device = *deviceFlag
	opts.version = *versionFlag
	if opts.device == "" && len(pos) >= 1 {
		opts.device = pos[0]
	}
	if opts.version == "" && len(pos) >= 2 {
		opts.version = pos[1]
	}
	opts.outputDir = *outputFlag
	if opts.outputDir == "" {
		opts.outputDir = *outputFlagLong
	}
	if opts.outputDir == "" && len(pos) >= 3 {
		opts.outputDir = pos[2]
	}
	opts.boardconfig = *boardconfigFlag
	opts.images = imageFlag
	opts.tagsOnly = *tagsFlag
	opts.listOnly = *listFlag
	opts.keepGoing = *keepGoingFlag

	if opts.device == "" || opts.version == "" {
		usage()
	}
	if opts.outputDir == "" {
		opts.outputDir = "."
	}

	if err := run(&opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func findEntry(entries []ZipEntry, name string) (ZipEntry, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return ZipEntry{}, false
}

// manifestResult holds the outcome of resolving the requested components
// against the IPSW's BuildManifest.plist.
type manifestResult struct {
	paths      map[string][]string // component name -> exact path(s) inside the archive (several when a device covers multiple boards)
	available  []string            // all tags present in the selected identity's manifest, sorted
	board      string              // selected boardconfig ("j71sap+j71tap" when a device spans multiple boards)
	variant    string              // selected identity variant
	manifestOK bool
}

// resolveComponents tries to resolve each requested component to the exact
// file inside the IPSW. Modern firmwares use BuildManifest.plist (authoritative).
// Firmwares too old to carry one fall back to The Apple Wiki keys page
// (exact filenames per device/version) plus filename matching. A non-nil
// error means the boardconfig could not be pinned down and the user must
// pick one with -boardconfig.
func resolveManifestComponents(rzip *RemoteZip, opts *options, info *FirmwareInfo, comps []component) (*manifestResult, error) {
	wikiCandidates, werr := LookupBoardConfig(opts.device)
	if werr != nil {
		fmt.Fprintf(os.Stderr, "  warning: could not look up boardconfig on The Apple Wiki: %v\n", werr)
	}
	if len(wikiCandidates) > 1 {
		fmt.Fprintf(os.Stderr, "  note: The Apple Wiki lists multiple boardconfigs for %s: %s\n", opts.device, strings.Join(wikiCandidates, ", "))
	}

	boardsList := func(boards map[string]string) []string {
		var out []string
		for _, b := range boards {
			out = append(out, b)
		}
		sort.Strings(out)
		return out
	}

	// resolveBoards picks the boardconfigs from (in priority order) the
	// -boardconfig flag, The Apple Wiki device article, and the boards present
	// in the archive. Devices may span multiple boards sharing one IPSW
	// (e.g. iPad6,11 -> J71sAP and J71tAP); then every matching board is
	// returned and their component paths unioned. When strict is set
	// (BuildManifest exists, so resolution is authoritative) ambiguity is a
	// hard error; otherwise the tool degrades to unfiltered filename matching
	// with a note.
	resolveBoards := func(boards map[string]string, strict bool) ([]string, error) {
		fail := func(msg string) ([]string, error) {
			if strict {
				return nil, fmt.Errorf("%s; use -boardconfig to pick one", msg)
			}
			fmt.Fprintf(os.Stderr, "  note: %s; not filtering by board (use -boardconfig to narrow)\n", msg)
			return nil, nil
		}
		if opts.boardconfig != "" {
			return []string{opts.boardconfig}, nil
		}
		if len(wikiCandidates) > 0 {
			var inter []string
			for _, c := range wikiCandidates {
				if _, ok := boards[strings.ToLower(c)]; ok {
					inter = append(inter, c)
				}
			}
			if len(inter) == 1 {
				return inter, nil
			}
			if len(inter) > 1 {
				fmt.Fprintf(os.Stderr, "  note: device %q covers multiple boards in this IPSW (%s); downloading components for each (use -boardconfig to pick one)\n", opts.device, strings.Join(inter, ", "))
				return inter, nil
			}
			if len(wikiCandidates) == 1 {
				return []string{wikiCandidates[0]}, nil
			}
			return fail(fmt.Sprintf("could not match device %q to a board in this IPSW (wiki candidates: %s; boards: %s)",
				opts.device, strings.Join(wikiCandidates, ", "), strings.Join(boardsList(boards), ", ")))
		}
		if len(boards) == 1 {
			for _, b := range boards {
				return []string{b}, nil
			}
		}
		return fail(fmt.Sprintf("could not determine boardconfig for %q (boards: %s)",
			opts.device, strings.Join(boardsList(boards), ", ")))
	}

	// --- Try the BuildManifest.plist path first. ---
	if manifestEntry, ok := findEntry(rzip.Entries, "BuildManifest.plist"); ok {
		fmt.Println("Found BuildManifest.plist; resolving exact component paths...")
		buf := new(bytes.Buffer)
		if err := rzip.Download(manifestEntry, buf, nil); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not download BuildManifest.plist: %v\n", err)
			return &manifestResult{}, nil
		}

		root, err := parsePlistXML(buf.Bytes())
		var identities []interface{}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not parse BuildManifest.plist: %v\n", err)
		} else {
			rootDict := plistDict(root)
			if v := dictGetCI(rootDict, "ProductBuildVersion"); v != nil {
				if build, ok := v.(string); ok && build != "" && !strings.EqualFold(build, opts.expectedBuild) {
					fmt.Fprintf(os.Stderr, "  warning: BuildManifest build %s does not match selected build %s\n", build, opts.expectedBuild)
				}
			}
			identities = plistArray(dictGetCI(rootDict, "BuildIdentities"))
			if len(identities) == 0 {
				fmt.Fprintln(os.Stderr, "  warning: BuildManifest.plist contains no build identities")
			}
		}

		if len(identities) > 0 {
			// Distinct boardconfigs declared in the archive.
			archiveBoards := map[string]string{}
			for _, iv := range identities {
				info := plistDict(dictGetCI(plistDict(iv), "Info"))
				if info == nil {
					continue
				}
				b := plistString(dictGetCI(info, "DeviceClass"))
				if b != "" {
					archiveBoards[strings.ToLower(b)] = b
				}
			}

			boards, err := resolveBoards(archiveBoards, true)
			if err != nil {
				return nil, err
			}
			fmt.Printf("Boardconfig for %s: %s\n", opts.device, strings.Join(boards, ", "))

			// Resolve the components for every selected board (a device may
			// span multiple boards sharing one IPSW).
			res := &manifestResult{paths: map[string][]string{}}
			var variantLabels []string
			for _, board := range boards {
				ident := selectIdentity(identities, board)
				if ident == nil {
					return nil, fmt.Errorf("no build identity for board %q in BuildManifest.plist (available boards: %s); use -boardconfig to pick one",
						board, strings.Join(boardsList(archiveBoards), ", "))
				}
				identInfo := plistDict(dictGetCI(ident, "Info"))
				fmt.Printf("Using build identity for board %s (%s)\n",
					plistString(dictGetCI(identInfo, "DeviceClass")), plistString(dictGetCI(identInfo, "Variant")))
				variant := plistString(dictGetCI(identInfo, "Variant"))
				seen := false
				for _, v := range variantLabels {
					if v == variant {
						seen = true
						break
					}
				}
				if !seen {
					variantLabels = append(variantLabels, variant)
				}

				manifest := plistDict(dictGetCI(ident, "Manifest"))
				if manifest == nil {
					fmt.Fprintln(os.Stderr, "  warning: build identity has no manifest; using filename matching for board", board)
					continue
				}
				if res.board == "" {
					res.board = board
				} else {
					res.board += "+" + board
				}
				res.manifestOK = true
				for _, c := range comps {
					if p, ok := findManifestTag(manifest, c.tag); ok {
						dup := false
						for _, existing := range res.paths[c.name] {
							if existing == p {
								dup = true
								break
							}
						}
						if !dup {
							res.paths[c.name] = append(res.paths[c.name], p)
						}
					}
				}
				// Only list tags that resolve to an actual file in this IPSW
				// (some tags are metadata-only).
				for k, v := range manifest {
					entry := plistDict(v)
					if entry == nil {
						continue
					}
					mInfo := plistDict(dictGetCI(entry, "Info"))
					if mInfo == nil || plistString(dictGetCI(mInfo, "Path")) == "" {
						continue
					}
					seenTag := false
					for _, t := range res.available {
						if t == k {
							seenTag = true
							break
						}
					}
					if !seenTag {
						res.available = append(res.available, k)
					}
				}
			}
			sort.Strings(res.available)
			res.variant = strings.Join(variantLabels, "/")
			if !res.manifestOK {
				return &manifestResult{board: res.board}, nil
			}
			return res, nil
		}
		fmt.Fprintln(os.Stderr, "  warning: falling back to The Apple Wiki metadata and filename matching")
	} else {
		fmt.Println("No BuildManifest.plist in archive (iOS 3 or earlier?); using The Apple Wiki metadata and filename matching")
	}

	// --- No (usable) BuildManifest.plist: ancient-firmware path. ---
	// Resolve the board from the filename layout of the archive (single-board
	// IPSWs resolve automatically; universal ones resolve via the wiki device
	// article, e.g. iPhone1,2 -> N82AP).
	zipBoards := map[string]string{}
	for _, b := range deriveZipBoards(rzip.Entries) {
		zipBoards[b] = b
	}
	boards, _ := resolveBoards(zipBoards, false)
	var board string
	if len(boards) == 1 {
		board = boards[0]
		fmt.Printf("Boardconfig for %s: %s\n", opts.device, board)
	} else if len(boards) > 1 {
		fmt.Fprintf(os.Stderr, "  note: multiple boards match in this IPSW (%s); not filtering (use -boardconfig to narrow)\n", strings.Join(boards, ", "))
	} else if len(zipBoards) > 0 {
		fmt.Fprintf(os.Stderr, "  note: boards present in archive: %s\n", strings.Join(boardsList(zipBoards), ", "))
	}

	res := &manifestResult{paths: map[string][]string{}, board: board}

	// Exact filenames from the keys page (the wiki lists these for ancient
	// firmwares, e.g. "RestoreRamdisk = 018-3783-2.dmg").
	for k, v := range wikiComponentPaths(info.PageData, rzip.Entries, comps) {
		res.paths[k] = []string{v}
	}

	// Build a usable -tags list: keys-page tags that resolve to a real file,
	// plus tags derived from the archive listing (iBSS/LLB/... aren't
	// encrypted, so the wiki doesn't know them).
	tagSet := map[string]string{}
	for _, t := range wikiResolvingTags(info.PageData, rzip.Entries) {
		tagSet[normalizeTag(t)] = t
	}
	for _, t := range deriveZipTags(rzip.Entries) {
		if _, ok := tagSet[normalizeTag(t)]; !ok {
			tagSet[normalizeTag(t)] = t
		}
	}
	for _, t := range tagSet {
		res.available = append(res.available, t)
	}
	sort.Strings(res.available)
	return res, nil
}

func run(opts *options) error {
	fmt.Printf("Looking up %s %s on The Apple Wiki...\n", opts.device, opts.version)
	info, err := LookupFirmware(opts.device, opts.version)
	if err != nil {
		return err
	}
	fmt.Printf("Found: iOS %s (%s), codename %q\n", info.Version, info.Build, info.Codename)
	fmt.Printf("IPSW: %s\n", info.DownloadURL)
	opts.expectedBuild = info.Build

	fmt.Println("Reading IPSW central directory...")
	rzip, err := OpenRemoteZip(info.DownloadURL)
	if err != nil {
		return err
	}
	fmt.Printf("Archive contains %d entries (%.1f MB)\n", len(rzip.Entries), float64(rzip.size)/(1024*1024))

	comps := requestedComponents(opts.images)
	explicit := len(opts.images) > 0

	mres, err := resolveManifestComponents(rzip, opts, info, comps)
	if err != nil {
		return err
	}

	// -tags: list the available image tags for this IPSW and exit.
	if opts.tagsOnly {
		if len(mres.available) == 0 {
			return fmt.Errorf("no downloadable image tags found in this IPSW")
		}
		if mres.manifestOK {
			fmt.Printf("Available tags for board %s (%s):\n", mres.board, mres.variant)
		} else {
			fmt.Printf("Available tags (no BuildManifest in archive; via The Apple Wiki and archive listing)%s:\n", boardSuffix(mres.board))
		}
		for _, tag := range mres.available {
			fmt.Printf("  %s\n", tag)
		}
		fmt.Printf("\nUse -image <tag> to download one (repeatable, comma-separated), e.g. -image RestoreRamDisk,LLB\n")
		return nil
	}

	byName := make(map[string]ZipEntry, len(rzip.Entries))
	for _, e := range rzip.Entries {
		byName[e.Name] = e
	}

	// Select entries per component.
	selected := make(map[string][]ZipEntry, len(comps))
	var missing []string
	for _, c := range comps {
		var matches []ZipEntry

		// Authoritative path(s) from BuildManifest.plist, when available.
		if paths, ok := mres.paths[c.name]; ok {
			for _, p := range paths {
				if e, ok := byName[p]; ok {
					matches = append(matches, e)
				} else {
					fmt.Fprintf(os.Stderr, "  warning: manifest path %q for %s not found in archive\n", p, c.name)
				}
			}
		}

		// Fall back to filename matching.
		if matches == nil {
			for _, e := range rzip.Entries {
				if e.IsDir() {
					continue
				}
				if !c.match(filepath.Base(e.Name)) {
					continue
				}
				matches = append(matches, e)
			}
			effectiveBoard := opts.boardconfig
			if effectiveBoard == "" {
				effectiveBoard = mres.board
			}
			if effectiveBoard != "" {
				var filtered []ZipEntry
				for _, e := range matches {
					if boardconfigMatch(e.Name, effectiveBoard) {
						filtered = append(filtered, e)
					}
				}
				if len(filtered) > 0 {
					matches = filtered
				}
			} else if len(matches) > 1 {
				fmt.Fprintf(os.Stderr, "  note: multiple %s matches in archive and no boardconfig resolved; downloading all of them (use -boardconfig to narrow)\n", c.name)
			}
		}

		selected[c.name] = matches
		if len(matches) == 0 {
			missing = append(missing, c.name)
		}
	}

	// Explicitly requested tags that can't be found are an error, listing
	// the available tags so the user can pick a valid one.
	if len(missing) > 0 && explicit {
		msg := fmt.Sprintf("requested tag(s) not found: %s", strings.Join(missing, ", "))
		if len(mres.available) > 0 {
			if mres.manifestOK {
				msg += fmt.Sprintf("\nAvailable tags for board %s (%s): %s",
					mres.board, mres.variant, strings.Join(mres.available, ", "))
			} else {
				msg += fmt.Sprintf("\nAvailable tags: %s", strings.Join(mres.available, ", "))
			}
		}
		if !opts.keepGoing {
			return fmt.Errorf("%s", msg)
		}
		fmt.Fprintln(os.Stderr, "warning: "+msg)
	}

	total := 0
	for _, c := range comps {
		matches := selected[c.name]
		if len(matches) == 0 {
			if !explicit {
				fmt.Printf("  warning: no %s found in IPSW\n", c.name)
			}
			continue
		}
		for _, e := range matches {
			fmt.Printf("  %-24s %s (%s compressed)\n", c.name, e.Name, humanSize(e.CompressedSize))
			total++
		}
	}
	if total == 0 {
		return fmt.Errorf("none of the requested components were found in the IPSW")
	}
	if opts.listOnly {
		return nil
	}

	if err := os.MkdirAll(opts.outputDir, 0o755); err != nil {
		return fmt.Errorf("could not create output directory %q: %w", opts.outputDir, err)
	}

	downloaded := 0
	var failures []string
	seenDst := map[string]string{}
	for _, c := range comps {
		for _, e := range selected[c.name] {
			base := filepath.Base(e.Name)
			dstPath := filepath.Join(opts.outputDir, base)
			if prev, dup := seenDst[dstPath]; dup {
				fmt.Printf("Skipping %s (%s and %s resolve to the same file)\n", base, prev, c.name)
				continue
			}
			seenDst[dstPath] = c.name

			fmt.Printf("Downloading %s (as %s)...\n", base, c.name)
			f, err := os.Create(dstPath)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", base, err))
				fmt.Fprintf(os.Stderr, "  error: %v\n", err)
				if !opts.keepGoing {
					return fmt.Errorf("failed to create %s: %w", dstPath, err)
				}
				continue
			}

			lastPercent := -1
			progress := func(done, totalB uint64) {
				percent := int(done * 100 / totalB)
				if percent != lastPercent && percent%10 == 0 {
					fmt.Printf("  %d%% (%s/%s)\n", percent, humanSize(done), humanSize(totalB))
					lastPercent = percent
				}
			}

			err = rzip.Download(e, f, progress)
			cerr := f.Close()
			if err != nil {
				os.Remove(dstPath)
				failures = append(failures, fmt.Sprintf("%s: %v", base, err))
				fmt.Fprintf(os.Stderr, "  error: %v\n", err)
				if !opts.keepGoing {
					return err
				}
				continue
			}
			if cerr != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", base, cerr))
				fmt.Fprintf(os.Stderr, "  error closing %s: %v\n", dstPath, cerr)
				if !opts.keepGoing {
					return cerr
				}
				continue
			}
			downloaded++
			fmt.Printf("  saved to %s\n", dstPath)
		}
	}

	fmt.Printf("Done: %d file(s) downloaded to %s\n", downloaded, opts.outputDir)
	if len(failures) > 0 {
		return fmt.Errorf("some downloads failed:\n  %s", strings.Join(failures, "\n  "))
	}
	return nil
}

// boardSuffix renders the resolved board for the tag-listing header.
func boardSuffix(board string) string {
	if board == "" {
		return ""
	}
	return " for board " + board
}

// humanSize formats a byte count like "15.3 MB".
func humanSize(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// IsDir reports whether the ZIP entry is a directory.
func (e ZipEntry) IsDir() bool {
	return strings.HasSuffix(e.Name, "/")
}
