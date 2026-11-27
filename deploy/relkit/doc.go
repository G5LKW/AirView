// Command relkit is the packaging and verification helper of AirView's
// release build (deploy/build.ps1, WP-BUILD). It does the parts of a release
// that must be exact and that PowerShell on an NTFS checkout cannot express:
//
//	relkit pack    -dir STAGE -o NAME.tar.gz [-mtime UNIX]
//	    Packs the directory STAGE as NAME.tar.gz with a single top-level
//	    directory (the base name of STAGE). Every entry is owned by root
//	    (uid/gid 0, uname/gname root), directories are 0755, regular files
//	    are 0755 when they are executable on Linux (ELF binaries and scripts
//	    starting with "#!") and 0644 otherwise. Entries are sorted, the time
//	    stamp is fixed (-mtime, else SOURCE_DATE_EPOCH, else now), and the
//	    gzip header carries no name or time: the same input gives the same
//	    bytes. Files on NTFS have no Unix modes, so the modes are derived
//	    from the content, never from the file system.
//
//	relkit sums    -dir DIR [-o DIR/SHA256SUMS] [-match REGEXP]
//	    Writes "sha256sum -c" compatible checksums (LF line ends, sorted) of
//	    the top-level regular files of DIR whose names match REGEXP (default:
//	    names starting with "airview", case-insensitive), except *.old,
//	    *.sha256 and the SHA256SUMS file itself.
//
//	relkit notices -pkg PKG -goos OS -goarch ARCH [-web DIR] -o FILE
//	    Writes THIRD-PARTY-NOTICES.txt for the modules linked into PKG for
//	    that target (go list -deps; licences read from the local module
//	    cache, nothing is downloaded), plus the Go licence and, with -web,
//	    the LICENSE* files below DIR (web/static/vendor). A module without a
//	    licence file stops the release.
//
//	relkit check   -dir DIR [-version V] [-require a,b,...] [-sums=true]
//	    Verifies a release directory and lists every artifact: SHA256SUMS
//	    (every listed file exists and matches; every artifact is listed),
//	    every .tar.gz (one top-level directory named like the archive, root
//	    ownership, modes as pack sets them, no CRLF in scripts or sourced
//	    *.sh libraries, statically linked ELF binaries for the CPU in their
//	    name, VERSION file equal to -version), every .zip (all members read
//	    back, CRC-32 checked, .exe members' PE machine), every .exe (PE
//	    machine matches the name), every NAME.sha256 (matches NAME), every
//	    top-level .sh (#! line, LF) and the -require list (every name must
//	    exist; with it, SHA256SUMS must list exactly these and other files in
//	    the directory are shown as "extra"). Exit status 1 when anything is
//	    wrong.
//
// relkit uses the standard library and airview packages only (no
// third-party modules). Tests: relkit_test.go builds bundles in a temporary
// directory and checks that every rule above catches its violation.
package main
