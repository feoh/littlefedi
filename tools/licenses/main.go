// Command licenses emits THIRD_PARTY_LICENSES.md: the verbatim licence text of
// every module linked into a release binary, plus the web assets bundled into
// the server. MIT, BSD, and Apache-2.0 all require the notice to travel with
// the distributed binary, so the generated file is part of a release.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Release binaries are built from these tag sets (see the Makefile). The union
// of their dependency graphs is what ships.
var tagSets = []string{"blog", "postgres blog", "s3 blog", "postgres s3 blog"}

// The SQLite driver is chosen by GOOS/GOARCH, not by a build tag: platforms
// modernc.org/sqlite supports get it, the rest fall back to
// github.com/ncruces/go-sqlite3. Both drivers ship across the release matrix,
// so each tag set is listed once per branch.
var platforms = []struct{ GOOS, GOARCH string }{
	{"linux", "amd64"},  // modernc.org/sqlite
	{"netbsd", "arm64"}, // ncruces/go-sqlite3 fallback
}

// Assets vendored under internal/webui/static that are not Go modules. Their
// licence text is held here because the files themselves carry none: the
// scripts are minified and the font is a binary.
var bundled = []struct{ Name, Version, Origin, Licence, Text string }{
	{
		Name:    "Alpine.js",
		Version: "3.15.12",
		Origin:  "https://github.com/alpinejs/alpine",
		Licence: "MIT",
		Text: `Copyright © 2019-2025 Caleb Porzio and contributors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.`,
	},
	{
		Name:    "htmx",
		Version: "2.0.10",
		Origin:  "https://github.com/bigskysoftware/htmx",
		Licence: "0BSD",
		Text: `Permission to use, copy, modify, and/or distribute this software for
any purpose with or without fee is hereby granted.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL
WARRANTIES WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES
OF MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE
FOR ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY
DAMAGES WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN
AN ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT
OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.`,
	},
	{
		Name:    "gemoji",
		Version: "shortcode names only",
		Origin:  "https://github.com/github/gemoji",
		Licence: "MIT",
		Text: `Copyright (c) GitHub, Inc.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.`,
	},
	{
		Name:    "Bricolage Grotesque",
		Version: "1.001, latin subset, weight axis only",
		Origin:  "https://github.com/ateliertriay/bricolage",
		Licence: "SIL OFL 1.1",
		Text: `Copyright 2022 The Bricolage Grotesque Project Authors (https://github.com/ateliertriay/bricolage)

This Font Software is licensed under the SIL Open Font License, Version 1.1.
This license is copied below, and is also available with a FAQ at:
https://scripts.sil.org/OFL


-----------------------------------------------------------
SIL OPEN FONT LICENSE Version 1.1 - 26 February 2007
-----------------------------------------------------------

PREAMBLE
The goals of the Open Font License (OFL) are to stimulate worldwide
development of collaborative font projects, to support the font creation
efforts of academic and linguistic communities, and to provide a free and
open framework in which fonts may be shared and improved in partnership
with others.

The OFL allows the licensed fonts to be used, studied, modified and
redistributed freely as long as they are not sold by themselves. The
fonts, including any derivative works, can be bundled, embedded, 
redistributed and/or sold with any software provided that any reserved
names are not used by derivative works. The fonts and derivatives,
however, cannot be released under any other type of license. The
requirement for fonts to remain under this license does not apply
to any document created using the fonts or their derivatives.

DEFINITIONS
"Font Software" refers to the set of files released by the Copyright
Holder(s) under this license and clearly marked as such. This may
include source files, build scripts and documentation.

"Reserved Font Name" refers to any names specified as such after the
copyright statement(s).

"Original Version" refers to the collection of Font Software components as
distributed by the Copyright Holder(s).

"Modified Version" refers to any derivative made by adding to, deleting,
or substituting -- in part or in whole -- any of the components of the
Original Version, by changing formats or by porting the Font Software to a
new environment.

"Author" refers to any designer, engineer, programmer, technical
writer or other person who contributed to the Font Software.

PERMISSION & CONDITIONS
Permission is hereby granted, free of charge, to any person obtaining
a copy of the Font Software, to use, study, copy, merge, embed, modify,
redistribute, and sell modified and unmodified copies of the Font
Software, subject to the following conditions:

1) Neither the Font Software nor any of its individual components,
in Original or Modified Versions, may be sold by itself.

2) Original or Modified Versions of the Font Software may be bundled,
redistributed and/or sold with any software, provided that each copy
contains the above copyright notice and this license. These can be
included either as stand-alone text files, human-readable headers or
in the appropriate machine-readable metadata fields within text or
binary files as long as those fields can be easily viewed by the user.

3) No Modified Version of the Font Software may use the Reserved Font
Name(s) unless explicit written permission is granted by the corresponding
Copyright Holder. This restriction only applies to the primary font name as
presented to the users.

4) The name(s) of the Copyright Holder(s) or the Author(s) of the Font
Software shall not be used to promote, endorse or advertise any
Modified Version, except to acknowledge the contribution(s) of the
Copyright Holder(s) and the Author(s) or with their explicit written
permission.

5) The Font Software, modified or unmodified, in part or in whole,
must be distributed entirely under this license, and must not be
distributed under any other license. The requirement for fonts to
remain under this license does not apply to any document created
using the Font Software.

TERMINATION
This license becomes null and void if any of the above conditions are
not met.

DISCLAIMER
THE FONT SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO ANY WARRANTIES OF
MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT
OF COPYRIGHT, PATENT, TRADEMARK, OR OTHER RIGHT. IN NO EVENT SHALL THE
COPYRIGHT HOLDER BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY,
INCLUDING ANY GENERAL, SPECIAL, INDIRECT, INCIDENTAL, OR CONSEQUENTIAL
DAMAGES, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
FROM, OUT OF THE USE OR INABILITY TO USE THE FONT SOFTWARE OR FROM
OTHER DEALINGS IN THE FONT SOFTWARE.`,
	},
}

type pkg struct {
	Standard bool
	Module   *struct {
		Path, Version, Dir string
		Main               bool
	}
}

type mod struct {
	Path, Version, Dir string
}

func main() {
	mods := map[string]mod{}
	for _, tags := range tagSets {
		for _, p := range platforms {
			for _, m := range listModules(tags, p.GOOS, p.GOARCH) {
				mods[m.Path] = m
			}
		}
	}

	paths := make([]string, 0, len(mods))
	for p := range mods {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := &strings.Builder{}
	fmt.Fprintf(out, `# Third-party licences

littleFedi is distributed under the MIT licence (see LICENSE). The release
binaries statically link the Go modules below and embed the web assets listed
at the end. Their licences are reproduced here in full, as those licences
require.

Generated by %s on %s. Do not edit by hand.

## Go modules

`, "`go run ./tools/licenses`", time.Now().UTC().Format("2006-01-02"))

	var missing []string
	for _, p := range paths {
		m := mods[p]
		files := licenceFiles(m.Dir)
		if len(files) == 0 {
			missing = append(missing, p)
			continue
		}
		fmt.Fprintf(out, "### %s %s\n\n", m.Path, m.Version)
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				fatal(err)
			}
			if len(files) > 1 {
				fmt.Fprintf(out, "%s:\n\n", filepath.Base(f))
			}
			fmt.Fprintf(out, "```\n%s\n```\n\n", strings.TrimRight(string(b), "\n"))
		}
	}

	fmt.Fprint(out, "## Bundled web assets\n\n")
	for _, a := range bundled {
		fmt.Fprintf(out, "### %s %s (%s)\n\n%s\n\n```\n%s\n```\n\n",
			a.Name, a.Version, a.Licence, a.Origin, a.Text)
	}

	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "no licence file found for: %s\n", strings.Join(missing, ", "))
		os.Exit(1)
	}
	if _, err := io.WriteString(os.Stdout, out.String()); err != nil {
		fatal(err)
	}
}

// listModules returns every non-stdlib, non-main module reachable from the
// server binary built with the given tags for the given platform.
func listModules(tags, goos, goarch string) []mod {
	cmd := exec.Command("go", "list", "-deps", "-json", "-tags", tags, "./cmd/littlefedi")
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fatal(fmt.Errorf("go list -tags %q for %s/%s: %w", tags, goos, goarch, err))
	}
	var mods []mod
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p pkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			fatal(err)
		}
		if p.Standard || p.Module == nil || p.Module.Main || p.Module.Dir == "" {
			continue
		}
		mods = append(mods, mod{p.Module.Path, p.Module.Version, p.Module.Dir})
	}
	return mods
}

// licenceFiles returns the licence-bearing files at the root of a module
// directory. Go's own modules split the grant across LICENSE and PATENTS, so
// every match is reproduced, not just the first.
func licenceFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		fatal(err)
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch name := strings.ToUpper(e.Name()); {
		case strings.HasPrefix(name, "LICENSE"),
			strings.HasPrefix(name, "LICENCE"),
			strings.HasPrefix(name, "COPYING"),
			strings.HasPrefix(name, "NOTICE"),
			name == "PATENTS":
			found = append(found, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(found)
	return found
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "licenses:", err)
	os.Exit(1)
}
