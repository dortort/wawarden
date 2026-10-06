package archtest

import (
	"go/ast"
	"go/token"
	"regexp"
	"strings"
)

var tokenText = regexp.MustCompile(`ww(adm_|_[a-z2-7]{8}_)[A-Za-z0-9_-]{43}_[0-9a-f]{8}`)

var (
	tokenShaped       = "wwadm_" + strings.Repeat("A", 43) + "_00000000"
	clientTokenShaped = "ww_aaaqeaye_" + strings.Repeat("A", 43) + "_00000000"
)

var credentialRule = rule{
	name:  "credential-literals",
	check: checkCredentialLiterals,
	cases: []snippet{
		{name: "whole admin tokens in literals and comments", rel: "internal/token/x_test.go", want: 4, src: "package token\n\n// " + tokenShaped + "\nconst a = \"" + tokenShaped + "\"\n\nvar b = `prefix " + tokenShaped + " suffix`\n\nvar c = 'x' /* " + tokenShaped + " */\n"},
		{name: "whole client tokens in literals and comments", rel: "internal/store/admin/x_test.go", want: 4, src: "package admin\n\n// " + clientTokenShaped + "\nconst a = \"" + clientTokenShaped + "\"\n\nvar b = `Bearer " + clientTokenShaped + "`\n\nvar c = 'x' /* " + clientTokenShaped + " */\n"},
		{name: "tokens assembled from parts", rel: "internal/token/x_test.go", src: `package token

import (
	"regexp"
	"strings"
)

const a = "wwadm_" + "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" + "_c307c63e"

var b = "wwadm_" + strings.Repeat("_", 42) + "8" + "_384c24f2"

const d = "ww_aaaqeaye_" + "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" + "_46e2849b"

var format = regexp.MustCompile(` + "`^wwadm_[A-Za-z0-9_-]{43}_[0-9a-f]{8}$`" + `)

var clientFormat = regexp.MustCompile(` + "`^ww_[a-z2-7]{8}_[A-Za-z0-9_-]{43}_[0-9a-f]{8}$`" + `)

// wwadm_ followed by 43 base64url characters, an underscore and 8 hex digits
var c = "wwadm_x_00000000"

var e = "ww_AAAQEAYE_" + "x_00000000"
`},
	},
}

func checkCredentialLiterals(f *sourceFile) []string {
	const msg = "a whole admin or client token in source reads as a live credential to secret scanners: assemble test vectors from parts"
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && tokenText.MatchString(lit.Value) {
			out = append(out, f.at(lit, msg))
		}
		return true
	})
	for _, group := range f.file.Comments {
		for _, c := range group.List {
			if tokenText.MatchString(c.Text) {
				out = append(out, f.at(c, msg))
			}
		}
	}
	return out
}
