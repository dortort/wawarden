package archtest

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	apiDir      = "internal/api"
	apiAdminDir = "internal/api/admin"
	appDir      = "internal/app"
	policyDir   = "internal/policy"
)

var bannedImports = set("net/http/pprof", "expvar", "plugin", "unsafe", "C")

var apiDenied = []string{"database/sql", "modernc.org/sqlite", "go.mau.fi/whatsmeow", module + "/internal/store/ingest"}

var policyAllowed = set("bytes", "cmp", "crypto/sha256", "crypto/subtle", "encoding/base64", "encoding/binary", "encoding/hex",
	"errors", "hash/crc32", "iter", "maps", "slices", "sort", "strconv", "strings", "time", "unicode", "unicode/utf8")

var allowedModules []string

var bannedImportRule = rule{
	name:  "banned-imports",
	check: checkBannedImports,
	cases: []snippet{
		{name: "every banned import in a test", rel: "internal/app/x_test.go", want: 5, src: `package app

import (
	"C"
	_ "expvar"
	_ "net/http/pprof"
	"plugin"
	"unsafe"
)
`},
		{name: "a banned import under an alias", rel: "cmd/wawarden/x.go", want: 1, src: `package main

import p "plugin"
`},
		{name: "neighbouring packages", rel: "internal/app/x.go", src: `package app

import (
	"net/http"
	"runtime/pprof"
	"unicode/utf8"
)
`},
	},
}

func checkBannedImports(f *sourceFile) []string {
	var out []string
	for _, imp := range f.imports {
		if bannedImports[imp.path] {
			out = append(out, f.at(imp.node, "import of %q is banned", imp.path))
		}
	}
	return out
}

var fenceRule = rule{
	name:  "import-fences",
	check: checkFences,
	cases: []snippet{
		{name: "api imports past its fence", rel: "internal/api/x.go", want: 8, src: `package api

import (
	"database/sql"
	"database/sql/driver"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"modernc.org/sqlite"
	"modernc.org/sqlite/lib"
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"
)
`},
		{name: "an api subpackage imports past the fence", rel: "internal/api/dto/x.go", want: 2, src: `package dto

import (
	"database/sql"
	"github.com/dortort/wawarden/internal/store/admin/sub"
)
`},
		{name: "api/admin imports the admin store", rel: "internal/api/admin/x.go", src: `package admin

import "github.com/dortort/wawarden/internal/store/admin"
`},
		{name: "outside api", rel: "internal/apiary/x.go", src: `package apiary

import (
	"database/sql"
	"github.com/dortort/wawarden/internal/store/ingest"
)
`},
		{name: "an api test", rel: "internal/api/x_test.go", src: `package api

import "database/sql"
`},
		{name: "policy imports I/O and non-standard packages", rel: "internal/policy/x.go", want: 19, src: `package policy

import (
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"example.com/lib"
	"fmt"
	"github.com/dortort/wawarden/internal/token"
	"io/fs"
	"io/ioutil"
	"log"
	"log/syslog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"syscall"
)
`},
		{name: "policy imports pure standard packages", rel: "internal/policy/x.go", src: `package policy

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"maps"
	"slices"
	"time"
)
`},
		{name: "a policy test imports the module", rel: "internal/policy/x_test.go", src: `package policy

import (
	"os"
	"github.com/dortort/wawarden/internal/token"
)
`},
		{name: "listeners opened outside the app", rel: "cmd/wawarden/x.go", want: 2, src: `package main

import (
	"github.com/dortort/wawarden/internal/listeners"
	"github.com/dortort/wawarden/internal/listeners/listenertest"
)
`},
		{name: "the app opens the listeners", rel: "internal/app/x.go", src: `package app

import "github.com/dortort/wawarden/internal/listeners"
`},
		{name: "a test inspects the listeners", rel: "internal/apiary/x_test.go", src: `package apiary

import "github.com/dortort/wawarden/internal/listeners/listenertest"
`},
	},
}

func checkFences(f *sourceFile) []string {
	if f.test {
		return nil
	}
	var out []string
	for _, imp := range f.imports {
		if within(imp.path, module+"/"+listenersDir) && f.dir != appDir && !within(f.dir, listenersDir) {
			out = append(out, f.at(imp.node, "only %s may import %q, so the app's inventory names every listener", appDir, imp.path))
		}
		switch {
		case within(f.dir, apiDir):
			if slices.ContainsFunc(apiDenied, func(denied string) bool { return within(imp.path, denied) }) ||
				within(imp.path, module+"/internal/store/admin") && !within(f.dir, apiAdminDir) {
				out = append(out, f.at(imp.node, "%s may not import %q", apiDir, imp.path))
			}
		case within(f.dir, policyDir):
			if !policyAllowed[imp.path] {
				out = append(out, f.at(imp.node, "%s may import only the reviewed pure standard-library packages, not %q", policyDir, imp.path))
			}
		}
	}
	return out
}

var standardLibrary = sync.OnceValues(func() (map[string]bool, error) {
	out, err := exec.CommandContext(context.Background(), "go", "list", "std").Output()
	if err != nil {
		return nil, err
	}
	return set(strings.Fields(string(out))...), nil
})

var thirdPartyRule = rule{
	name:  "third-party-imports",
	check: thirdPartyImports(allowedModules),
	cases: []snippet{
		{name: "third-party imports", rel: "internal/app/x_test.go", want: 4, src: `package app

import (
	"golang.org/x/tools/go/packages"
	"github.com/dortort/wawarden-fork/internal/policy"
	"evil/pkg"
	"fmtx"
)
`},
		{name: "standard library and this module", rel: "internal/app/x.go", src: `package app

import (
	"fmt"
	"net/http"
	"github.com/dortort/wawarden"
	"github.com/dortort/wawarden/internal/policy"
)
`},
	},
}

func thirdPartyImports(allowed []string) func(*sourceFile) []string {
	return func(f *sourceFile) []string {
		std, err := standardLibrary()
		if err != nil {
			return []string{f.at(f.file, "go list std: %v", err)}
		}
		var out []string
		for _, imp := range f.imports {
			if std[imp.path] || within(imp.path, module) || slices.ContainsFunc(allowed, func(m string) bool { return within(imp.path, m) }) {
				continue
			}
			out = append(out, f.at(imp.node, "%q is outside the standard library and this module, and not on the reviewed allow-list", imp.path))
		}
		return out
	}
}

func TestThirdPartyAllowList(t *testing.T) {
	f, err := parseSource("internal/app/x.go", []byte(`package app

import (
	"example.com/allowed"
	"example.com/allowed/sub"
	"example.com/allowedx"
	"example.com/other"
)
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := thirdPartyImports([]string{"example.com/allowed"})(f); len(got) != 2 {
		t.Fatalf("findings %q, want exactly example.com/allowedx and example.com/other", got)
	}
}
