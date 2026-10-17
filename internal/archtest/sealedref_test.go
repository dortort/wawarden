package archtest

import "go/ast"

const (
	sealedRefType    = "SealedRef"
	attachmentType   = "Attachment"
	attachmentLookup = "Attachment"
	mediaServiceType = "MediaService"
	sealedText       = "Sealed"
	refField         = "Ref"
	mediaServiceCall = "Fetch"
	mediaServiceFile = appDir + "/media.go"
	engineType       = "Engine"
)

var sealedRefRule = rule{
	name:  "sealed-ref",
	check: checkSealedRef,
	cases: []snippet{
		{name: "the sealed text read in the handlers", rel: apiDir + "/x.go", want: 2, src: `package api

func f(s *reads) {
	a, _, _ := s.archive.Attachment(g, ctx, chat, id, sender, now)
	_ = a.Ref.Sealed()
}
`},
		{name: "the reference logged, formatted or kept", rel: apiDir + "/x.go", want: 4, src: `package api

import (
	"fmt"
	"log/slog"

	"github.com/dortort/wawarden/internal/store/scoped"
)

type job struct{ ref scoped.SealedRef }

func f(s *reads) {
	a, found, err := s.archive.Attachment(g, ctx, chat, id, sender, now)
	slog.Info("attachment", "ref", a.Ref)
	_ = job{ref: a.Ref}
	_ = fmt.Sprint(a)
	_, _ = found, err
}
`},
		{name: "the reference returned from a closure", rel: apiDir + "/x.go", want: 2, src: `package api

import "github.com/dortort/wawarden/internal/store/scoped"

func f(s *reads) {
	a, _, _ := s.archive.Attachment(g, ctx, chat, id, sender, now)
	ref := func() scoped.SealedRef { return a.Ref }
	_ = ref
}
`},
		{name: "a looked-up attachment passed on or stored whole", rel: apiDir + "/x.go", want: 4, src: `package api

import "github.com/dortort/wawarden/internal/store/scoped"

var kept scoped.Attachment

func f(s *reads, a scoped.Attachment) {
	g(a)
	kept = a
	var x struct{ att scoped.Attachment }
	x.att, _, _ = s.archive.Attachment(g, ctx, chat, id, sender, now)
	b, _, _ := s.archive.Attachment(g, ctx, chat, id, sender, now)
	_ = &b
}
`},
		{name: "a sealed reference outside the media service's parameters", rel: apiDir + "/x.go", want: 3, src: `package api

import (
	"context"

	"github.com/dortort/wawarden/internal/store/scoped"
)

type ref = scoped.SealedRef

type MediaService interface {
	Fetch(ctx context.Context, ref scoped.SealedRef, req MediaFetch) (scoped.SealedRef, error)
}

func keep(r scoped.SealedRef) {}
`},
		{name: "the reference handed straight to the media service", rel: apiDir + "/x.go", src: `package api

import (
	"context"

	"github.com/dortort/wawarden/internal/store/scoped"
)

type MediaService interface {
	Precheck(declared int64) error
	Fetch(ctx context.Context, ref scoped.SealedRef, req MediaFetch) (MediaSpool, error)
}

func start(s *reads) {
	a, found, err := s.archive.Attachment(g, ctx, chat, id, sender, now)
	if err != nil || !found || !a.Usable {
		return
	}
	req := MediaFetch{Chat: a.Chat, ID: a.ID, Sender: a.Sender, Kind: a.Kind, Declared: a.Declared}
	s.media.admit(func(ctx context.Context) (MediaSpool, error) {
		return s.media.service.Fetch(ctx, a.Ref, req)
	})
	c, _, _ := s.archive.Chat(g, ctx, ref)
	_ = c.Ref
}

func (m *mediaRegistry) eligible(a scoped.Attachment) bool { return a.Usable }
`},
		{name: "a test in the handlers", rel: apiDir + "/x_test.go", src: `package api

func f(s *reads) {
	a, _, _ := s.archive.Attachment(g, ctx, chat, id, sender, now)
	_ = a.Ref.Sealed()
}
`},
		{name: "the media service opening the reference", rel: mediaServiceFile, src: `package app

func (s *mediaService) Fetch(ctx context.Context, ref scoped.SealedRef, req api.MediaFetch) {
	_ = engine.MediaRequest{Ref: ref.Sealed()}
}
`},
	},
}

var mediaFetchCallerRule = rule{
	name:  "media-fetch-callers",
	check: checkMediaFetchCallers,
	cases: []snippet{
		{name: "the engine fetching outside its media entry point", rel: engineDir + "/x.go", want: 2, src: `package engine

func (e *Engine) onMessage(ctx context.Context, req MediaRequest) {
	_, _ = e.sup.client.FetchMedia(ctx, req)
	_, _ = e.FetchMedia(ctx, req)
}
`},
		{name: "a fetch from another package", rel: apiDir + "/x.go", want: 1, src: `package api

func f(e fetcher) { _, _ = e.FetchMedia(ctx, req) }
`},
		{name: "a fetch from another app file", rel: appDir + "/x.go", want: 1, src: `package app

func f(e *engine.Engine) { fetch := e.FetchMedia; _ = fetch }
`},
		{name: "a fetch from a plain function named like the entry point", rel: engineDir + "/x.go", want: 1, src: `package engine

func FetchMedia(c Client) { _, _ = c.FetchMedia(ctx, req) }
`},
		{name: "the engine's entry point and the media service", rel: engineDir + "/x.go", src: `package engine

type Client interface {
	FetchMedia(ctx context.Context, req MediaRequest) (Staged, error)
}

func (e *Engine) FetchMedia(ctx context.Context, req MediaRequest) (Staged, error) {
	return e.sup.client.FetchMedia(ctx, req)
}
`},
		{name: "the app's media service", rel: mediaServiceFile, src: `package app

func (s *mediaService) Fetch(ctx context.Context) { _, _ = s.fetcher.FetchMedia(ctx, engine.MediaRequest{}) }
`},
		{name: "the adapter, the dev fake and tests", rel: adapterDir + "/x.go", src: `package wa

func (c *Client) FetchMedia(ctx context.Context, req engine.MediaRequest) (engine.Staged, error) { return c.FetchMedia(ctx, req) }
`},
		{name: "the dev fake", rel: fakeDir + "/x.go", src: `package fake

func (c *Client) FetchMedia(ctx context.Context, req engine.MediaRequest) (engine.Staged, error) { return nil, nil }
`},
		{name: "a test", rel: appDir + "/x_test.go", src: `package app

func f(e *engine.Engine) { _, _ = e.FetchMedia(ctx, req) }
`},
	},
}

func checkSealedRef(f *sourceFile) []string {
	if f.test || f.dir != apiDir {
		return nil
	}
	scoped := module + "/" + scopedDir
	allowed := mediaServiceParams(f.file)
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == sealedText {
			out = append(out, f.at(sel, "the handlers never read a sealed reference's text: only the media service opens it"))
		}
		if s, p := f.ref(sel); s != nil && p == scoped && s.Sel.Name == sealedRefType && !inSpans(allowed, sel) {
			out = append(out, f.at(sel, "scoped.%s may be named in %s only as a parameter of a %s method", sealedRefType, apiDir, mediaServiceType))
		}
		return true
	})
	for _, decl := range f.file.Decls {
		out = append(out, f.attachmentUses(decl, scoped)...)
	}
	return out
}

func mediaServiceParams(file *ast.File) []ast.Node {
	var spans []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name.Name != mediaServiceType {
			return true
		}
		if iface, ok := spec.Type.(*ast.InterfaceType); ok {
			for _, m := range iface.Methods.List {
				if ft, ok := m.Type.(*ast.FuncType); ok {
					spans = append(spans, ft.Params)
				}
			}
		}
		return false
	})
	return spans
}

func inSpans(spans []ast.Node, n ast.Node) bool {
	for _, s := range spans {
		if s.Pos() <= n.Pos() && n.End() <= s.End() {
			return true
		}
	}
	return false
}

func (f *sourceFile) attachmentUses(decl ast.Decl, scoped string) []string {
	var out []string
	tracked := map[string]bool{}
	defined := map[*ast.Ident]bool{}
	track := func(id *ast.Ident) {
		if id.Name != "_" {
			tracked[id.Name] = true
			defined[id] = true
		}
	}
	ast.Inspect(decl, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Rhs) == 1 && isLookup(n.Rhs[0]) {
				if id, ok := n.Lhs[0].(*ast.Ident); ok {
					track(id)
				} else {
					out = append(out, f.at(n.Lhs[0], "an attachment from the scoped lookup is held only in a local variable"))
				}
			}
		case *ast.ValueSpec:
			if len(n.Values) == 1 && isLookup(n.Values[0]) {
				track(n.Names[0])
			}
		case *ast.FuncType:
			if n.Params != nil {
				for _, p := range n.Params.List {
					if f.isType(p.Type, scoped, attachmentType) {
						for _, name := range p.Names {
							track(name)
						}
					}
				}
			}
		}
		return true
	})
	if len(tracked) == 0 {
		return out
	}
	var stack []ast.Node
	ast.Inspect(decl, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		defer func() { stack = append(stack, n) }()
		id, ok := n.(*ast.Ident)
		if !ok || !tracked[id.Name] || defined[id] || len(stack) == 0 {
			return true
		}
		switch p := stack[len(stack)-1].(type) {
		case *ast.SelectorExpr:
			switch {
			case p.Sel == id:
			case p.Sel.Name != refField:
			case len(stack) > 1 && fetchArgument(stack[len(stack)-2], p):
			default:
				out = append(out, f.at(p, "a sealed reference from the scoped lookup goes only straight into %s.%s", mediaServiceType, mediaServiceCall))
			}
			return true
		case *ast.KeyValueExpr:
			if p.Key == id {
				return true
			}
		}
		out = append(out, f.at(id, "an attachment from the scoped lookup is used only through its fields, so its sealed reference cannot leave the handler"))
		return true
	})
	return out
}

func isLookup(e ast.Expr) bool {
	call, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == attachmentLookup
}

func fetchArgument(parent ast.Node, arg ast.Expr) bool {
	call, ok := parent.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != mediaServiceCall {
		return false
	}
	for _, a := range call.Args {
		if a == arg {
			return true
		}
	}
	return false
}

func checkMediaFetchCallers(f *sourceFile) []string {
	if f.test || within(f.dir, adapterDir) || within(f.dir, fakeDir) || f.rel == mediaServiceFile {
		return nil
	}
	var out []string
	for _, decl := range f.file.Decls {
		entry := f.dir == engineDir && isEngineFetch(decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == mediaFetch && !entry {
				out = append(out, f.at(sel, "%s is reached only from the engine's own %s and from %s, behind the handlers' admission and budgets: anywhere else it could fetch an attachment no client asked for", mediaFetch, mediaFetch, mediaServiceFile))
			}
			return true
		})
	}
	return out
}

func isEngineFetch(decl ast.Decl) bool {
	fn, ok := decl.(*ast.FuncDecl)
	if !ok || fn.Name.Name != mediaFetch || fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == engineType
}
