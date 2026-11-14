# ipswfetch

Download any image from any Apple IPSW over HTTP Range requests — by default
the **iBSS, iBEC, DeviceTree and kernelcache**, or any other tagged image
(`-image RestoreRamDisk`, `-tags` to list them) — without downloading the
whole (often 5–10 GB) restore image.

The tool:

1. Looks up the device + version on The Apple Wiki (via the same MediaWiki
   API used by [wikiproxy-go](https://github.com/LukeZGD/wikiproxy-go)) and
   extracts the `DownloadURL` for the IPSW. The version may be a marketing
   version (`15.4`) or a build number (`19E241`).
2. Reads the remote ZIP's end-of-central-directory and central directory
   through HTTP Range requests (the same technique used by
   [partialZipBrowser](https://github.com/tihmstar/partialZipBrowser),
   reimplemented natively in Go) — this lists every file inside the IPSW
   without downloading it. Firmwares as old as iOS 1.1.3 are supported;
   they have no `BuildManifest.plist`, so the tool falls back to the keys
   page on The Apple Wiki (exact filenames per device/version, e.g.
   `RestoreRamdisk = 018-3783-2.dmg`) and to filename matching, with the
   boardconfig detected from the archive layout and the wiki device article.
   For iOS 4+ the components are resolved from `BuildManifest.plist`.
3. Resolves the exact file path for each component from the IPSW's
   `BuildManifest.plist`, picking the build identity matching the device's
   boardconfig. The boardconfig is taken from `-boardconfig` if given,
   otherwise from the "Identifiers" table of The Apple Wiki device article
   (e.g. `iPhone9,3` -> `D101AP`), and disambiguated against the boards
   actually present in the IPSW. If the manifest can't be used, it falls back
   to filename matching.
4. Downloads only the wanted entries with HTTP Range requests, decompresses
   them (Store/Deflate) and verifies the CRC32. Very old iOS builds are
   supported too (`.dfu` / `.img3` formats, e.g. iOS 9).

## Build

```
go build -o ipswfetch .
```

No dependencies outside the standard library.

## Usage

```
ipswfetch [flags] <device> <version> [output-dir]
ipswfetch -device iPhone9,3 -version 15.4 [-o output-dir]
```

Examples:

```
ipswfetch iPhone9,3 15.4                 # downloads to current folder
ipswfetch iPhone9,3 15.4 /tmp/restore    # downloads to /tmp/restore
ipswfetch iPhone9,3 19E241               # version may be a build number
ipswfetch -list iPhone9,3 15.4           # show what would be downloaded
ipswfetch -boardconfig d10ap iPhone9,3 15.4   # force a boardconfig
```

Flags:

| Flag | Description |
|------|-------------|
| `-o`, `-output <dir>` | output directory (default: current folder) |
| `-device` | device identifier, e.g. `iPhone9,3` |
| `-version` | marketing version (`15.4`) or build number (`19E241`) |
| `-boardconfig` | only download files matching this boardconfig (e.g. `d10ap`) |
| `-image` | download a specific image by BuildManifest tag instead of the default set (repeatable, comma-separated), e.g. `-image RestoreRamDisk,LLB` |
| `-tags` | list the image tags available in the IPSW for the selected board and exit |
| `-keys` | print the IV and Key for the requested images (from The Apple Wiki keys page) without downloading |
| `-json` | with `-keys`, print JSON instead of text |
| `-list` | only list matching entries, don't download |
| `-keep-going` | continue with other components if one fails |

## Downloading a specific image (`-image`)

Any image listed in the IPSW's `BuildManifest.plist` can be downloaded by its
**tag**, no filename needed — for example the restore ramdisk:

```
ipswfetch -image RestoreRamDisk iPhone9,3 15.4
ipswfetch -image RestoreRamDisk -image LLB iPhone9,3 15.4
ipswfetch -image RestoreRamDisk,LLB iPhone9,3 15.4
ipswfetch -list -image OS iPhone9,3 15.4      # show size before downloading
ipswfetch -tags iPhone9,3 15.4                # list all downloadable tags
```

Tags are matched case-insensitively and ignoring punctuation, so
`RestoreRamDisk`, `restoreramdisk` and `restore ram disk` are equivalent.
Two wiki-style aliases are built in: `rootfs` → `OS` and `SEPFirmware` → `SEP`.
Common tags: `iBSS`, `iBEC`, `iBoot`, `LLB`, `DeviceTree`, `KernelCache`,
`RestoreRamDisk`, `RestoreDeviceTree`, `RestoreKernelCache`, `SEP`, `RestoreSEP`,
`BasebandFirmware`, `AppleLogo`, `OS` (the system root filesystem), `Homer`,
`SystemVolume`, `RestoreTrustCache`, `StaticTrustCache`, `Ap,SystemVolumeCanonicalMetadata`.
Note that the big ones (`OS`, i.e. the full root filesystem) are several GB.
If a tag isn't found, the tool prints the tags that *are* available for your
board — run `-tags` to see them up front.

Without `-image`, the default set (iBSS, iBEC, DeviceTree, kernelcache) is
downloaded.

## Fetching the IV and Key (`-keys`)

`-keys` prints the IV, Key and KBAG for the requested images straight from
The Apple Wiki keys page — no download, no archive access at all:

```
ipswfetch -keys iPhone9,3 15.4              # iBSS/iBEC/DeviceTree/kernelcache
ipswfetch -keys -image iBoot,LLB iPhone9,3 15.4
ipswfetch -json -keys -image iBoot iPhone9,3 15.4   # machine-readable output
ipswfetch -keys iPhone1,2 2.0               # ancient firmware: everything listed
```

KBAG is printed as `IV || Key`, matching the wikiproxy convention. Tags that
the wiki lists as `unknown` / `not encrypted` are reported as such; tags that
aren't on the page at all are noted on stderr (and are absent from JSON
output). Note what the wiki lists varies by era: as of this writing, iOS 12+
keys pages (e.g. 12.5.7, 15.4) list only the boot-chain images
(iBSS/iBEC/iBoot/LLB/SEPFirmware), while older pages (e.g. 9.3.5, iOS 2.x)
list nearly everything (DeviceTree, kernelcache, RootFS, …).

If the boardconfig can't be determined unambiguously (device articles with
multiple board variants), the tool exits with an error listing the candidates
and the boards present in the IPSW — pass `-boardconfig` to choose. A special
case: when a device legitimately covers several boards in the same IPSW (e.g.
iPad6,11 -> `J71sAP` and `J71tAP`), the tool downloads the components for each
matching board and says so; `-boardconfig` narrows it back down.

## Credits

- **[wikiproxy-go](https://github.com/LukeZGD/wikiproxy-go)** by **LukeZGD**
  (MIT License) — the The Apple Wiki API client and wikitext parsing in
  `wiki.go` is derived from this project. See `NOTICE` and
  `LICENSES/wikiproxy-go-LICENSE`.
- **[partialZipBrowser](https://github.com/tihmstar/partialZipBrowser)** by
  **tihmstar** (LGPL-3.0) — the remote ZIP browsing technique (end-of-central
  -directory + central directory over HTTP Range, then per-entry range reads)
  this tool uses; `zip.go` is an independent Go reimplementation with the
  standard library (no code from partialZipBrowser or libfragmentzip is
  included). See `NOTICE` and `LICENSES/partialZipBrowser-LICENSE`.
- **[The Apple Wiki](https://theapplewiki.com)** contributors — for the IPSW
  download links and firmware keys pages this tool queries at runtime.

## License

ipswfetch is free software licensed under the **GNU General Public License
version 3** (GPLv3). See `LICENSE` for the full license text. Portions derived
from other works are covered by their respective license terms as described in
`NOTICE`. This program is distributed without any warranty.
