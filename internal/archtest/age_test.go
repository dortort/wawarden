package archtest

import (
	"go/ast"
	"maps"
	"slices"
	"strings"
)

const (
	ageModule = "filippo.io/age"
	backupDir = "internal/backup"
)

var ageAllowed = set("Encrypt", "ParseRecipients", "Recipient")

var ageRecipientOnlyRule = rule{
	name:  "age-recipient-only",
	check: checkAgeRecipientOnly,
	cases: []snippet{
		{name: "identities, decryption and other age packages", rel: backupDir + "/x.go", want: 6, src: `package backup

import (
	"filippo.io/age"
	a "filippo.io/age"
	"filippo.io/age/agessh"
	"filippo.io/age/armor"
)

var (
	_ = age.Decrypt
	_ = age.GenerateX25519Identity
	_ = a.ParseIdentities
	_ age.Identity
	_ = agessh.ParseIdentity
	_ = armor.NewWriter
)
`},
		{name: "encryption to a parsed recipient", rel: backupDir + "/x.go", src: `package backup

import (
	"io"
	"strings"

	"filippo.io/age"
)

func f(w io.Writer, s string) (io.WriteCloser, error) {
	rs, err := age.ParseRecipients(strings.NewReader(s))
	if err != nil {
		return nil, err
	}
	var r age.Recipient = rs[0]
	return age.Encrypt(w, r)
}
`},
		{name: "a test decrypts with a throwaway identity", rel: backupDir + "/x_test.go", src: `package backup

import "filippo.io/age"

var _ = age.Decrypt
var _ = age.GenerateX25519Identity
`},
		{name: "neighbouring import paths", rel: backupDir + "/x.go", src: `package backup

import _ "filippo.io/agex"
`},
	},
}

func checkAgeRecipientOnly(f *sourceFile) []string {
	if f.test {
		return nil
	}
	var out []string
	for _, imp := range f.imports {
		if within(imp.path, ageModule) && imp.path != ageModule {
			out = append(out, f.at(imp.node, "%q is refused: the service imports only %s, to encrypt to a recipient", imp.path, ageModule))
		}
	}
	ast.Inspect(f.file, func(n ast.Node) bool {
		if e, ok := n.(*ast.SelectorExpr); ok {
			if sel, p := f.ref(e); sel != nil && p == ageModule && !ageAllowed[sel.Sel.Name] {
				out = append(out, f.at(sel, "age.%s is refused: the service holds only a recipient, so it may use only %s", sel.Sel.Name, strings.Join(slices.Sorted(maps.Keys(ageAllowed)), ", ")))
			}
		}
		return true
	})
	return out
}
