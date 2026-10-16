package archtest

import (
	"go/ast"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const (
	mediasrcDir  = adapterDir + "/internal/mediasrc"
	mediasrcPath = module + "/" + mediasrcDir
	waE2EPath    = whatsmeowModule + "/proto/waE2E"

	historyNotification = "HistorySyncNotification"
	mediaBuilder        = "downloadable"
	mediaFetch          = "FetchMedia"
	historyDownload     = "DownloadHistory"
	clientBuilder       = "newClient"
	clientNew           = "New"
	mediaSeamType       = "mediaDownloader"
	mediaSeam           = "downloadMediaFile"
	historySeam         = "downloadToFile"
	ramStaging          = "newCapFile"
	diskStaging         = "Admit"
)

var (
	mediaMessages      = []string{"ImageMessage", "VideoMessage", "AudioMessage", "DocumentMessage", "StickerMessage"}
	seamFields         = map[string]string{"fetch": mediaFetch, "download": historyDownload}
	mediaSourceSurface = set("Source", "Source.m", "Wrap", "Source.Message")
)

var mediaDownloadRule = rule{
	name:  "media-download",
	check: checkMediaDownload,
	cases: []snippet{
		{name: "the adapter's two seams, its message builder and its staging", rel: "internal/engine/wa/x.go", src: `package wa

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"

	"github.com/dortort/wawarden/internal/engine/wa/internal/mediasrc"
)

type downloader func(ctx context.Context, cli *whatsmeow.Client, notif *waE2E.HistorySyncNotification, dst whatsmeow.File) error

type mediaDownloader func(ctx context.Context, cli *whatsmeow.Client, src mediasrc.Source, dst whatsmeow.File) error

type Client struct {
	download downloader
	fetch    mediaDownloader
}

func downloadToFile(ctx context.Context, cli *whatsmeow.Client, notif *waE2E.HistorySyncNotification, dst whatsmeow.File) error {
	return cli.DownloadToFile(ctx, notif, dst)
}

func downloadMediaFile(ctx context.Context, cli *whatsmeow.Client, src mediasrc.Source, dst whatsmeow.File) error {
	msg, ok := src.Message()
	if !ok {
		return nil
	}
	return cli.DownloadToFile(ctx, msg, dst)
}

func New() (*Client, error) { return newClient(downloadToFile, downloadMediaFile) }

func newClient(download downloader, fetch mediaDownloader) (*Client, error) {
	c := &Client{download: download}
	c.fetch = fetch
	return c, nil
}

func downloadable(kind string) mediasrc.Source {
	switch kind {
	case "image":
		return mediasrc.Wrap(&waE2E.ImageMessage{})
	case "video":
		return mediasrc.Wrap(&waE2E.VideoMessage{})
	case "audio":
		return mediasrc.Wrap(&waE2E.AudioMessage{})
	case "document":
		return mediasrc.Wrap(&waE2E.DocumentMessage{})
	}
	return mediasrc.Wrap(new(waE2E.StickerMessage))
}

func (c *Client) FetchMedia(ctx context.Context, kind string, declared int64) error {
	src := downloadable(kind)
	var staged stagedFile
	var err error
	if declared <= 16 {
		staged = newCapFile(declared, declared)
	} else if staged, err = c.spool.Admit(declared, nil); err != nil {
		return err
	}
	return c.fetch(ctx, nil, src, staged)
}

func (c *Client) DownloadHistory(ctx context.Context) error {
	notif := &waE2E.HistorySyncNotification{}
	staged := newCapFile(1, 1)
	return c.download(ctx, nil, notif, staged)
}
`},
		{name: "the engine calls FetchMedia through its client interface", rel: "internal/engine/x.go", src: `package engine

import "context"

func (e *Engine) FetchMedia(ctx context.Context, req MediaRequest) (Staged, error) {
	return e.sup.client.FetchMedia(ctx, req)
}
`},
		{name: "the adapter's tests build messages and drive the seams directly", rel: "internal/engine/wa/x_test.go", src: `package wa

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"

	"github.com/dortort/wawarden/internal/engine/wa/internal/mediasrc"
)

func TestSeams(t *testing.T) {
	var zero mediasrc.Source
	c := &Client{}
	c.fetch = downloadMediaFile
	_ = downloadMediaFile(context.Background(), nil, zero, nil)
	_ = c.fetch(context.Background(), nil, mediasrc.Wrap(&waE2E.ImageMessage{}), nil)
	_ = downloadable("image")
	_, _ = c.FetchMedia(context.Background(), MediaRequest{})
	_ = &waE2E.HistorySyncNotification{}
}
`},
		{name: "media messages and a history notification built outside their builders", rel: "internal/engine/wa/x.go", want: 8, src: `package wa

import "go.mau.fi/whatsmeow/proto/waE2E"

func receipt() {
	_ = &waE2E.ImageMessage{}
	_ = waE2E.VideoMessage{}
	_ = []*waE2E.AudioMessage{{}}
	_ = new(waE2E.DocumentMessage)
	_ = &waE2E.HistorySyncNotification{}
}

func (c *Client) FetchMedia() { _ = &waE2E.StickerMessage{} }

func (c *Client) DownloadHistory() { _ = &waE2E.ImageMessage{} }

func downloadable() { _ = new(waE2E.HistorySyncNotification) }
`},
		{name: "mediasrc.Wrap taken as a value, called in a function literal and called outside downloadable", rel: "internal/engine/wa/x.go", want: 3, src: `package wa

import (
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/dortort/wawarden/internal/engine/wa/internal/mediasrc"
)

func downloadable(m whatsmeow.DownloadableMessage) mediasrc.Source {
	wrap := mediasrc.Wrap
	later := func() { _ = mediasrc.Wrap(m) }
	later()
	_ = wrap
	return mediasrc.Wrap(m)
}

func onMessage(evt *events.Message) {
	_ = mediasrc.Wrap(evt.Message.GetImageMessage())
}
`},
		{name: "mediasrc imported outside the adapter's own directory", rel: "internal/api/x.go", want: 1, src: `package api

import "github.com/dortort/wawarden/internal/engine/wa/internal/mediasrc"

var _ = mediasrc.Wrap
`},
		{name: "mediasrc imported by a subpackage of the adapter", rel: "internal/engine/wa/sub/x.go", want: 1, src: `package sub

import "github.com/dortort/wawarden/internal/engine/wa/internal/mediasrc"

func downloadable() mediasrc.Source { return mediasrc.Wrap(nil) }
`},
		{name: "mediasrc imported under another name", rel: "internal/engine/wa/x.go", want: 1, src: `package wa

import src "github.com/dortort/wawarden/internal/engine/wa/internal/mediasrc"

func downloadable() src.Source { return src.Wrap(nil) }
`},
		{name: "mediasrc.Source in a variable, new, a literal, a conversion, an assertion, an alias, a receiver and an instantiation", rel: "internal/engine/wa/x.go", want: 8, src: `package wa

import (
	"go.mau.fi/whatsmeow"

	"github.com/dortort/wawarden/internal/engine/wa/internal/mediasrc"
)

var zero mediasrc.Source

type alias = mediasrc.Source

func (s mediasrc.Source) leak() {}

func forge(v any, m whatsmeow.DownloadableMessage) {
	_ = new(mediasrc.Source)
	_ = mediasrc.Source{}
	_ = mediasrc.Source(struct{ m whatsmeow.DownloadableMessage }{m})
	_, _ = v.(mediasrc.Source)
	_ = generic[mediasrc.Source]()
}

func generic[T any]() T {
	var t T
	return t
}
`},
		{name: "downloadable taken as a value, called in a function literal, called at receipt and kept in a variable", rel: "internal/engine/wa/x.go", want: 4, src: `package wa

var eager = downloadable

func (c *Client) FetchMedia(kind string) {
	build := downloadable
	later := func() { _ = downloadable(kind) }
	later()
	_ = build
	_ = downloadable(kind)
}

func (c *Client) onReceipt(kind string) { _ = downloadable(kind) }
`},
		{name: "the seam functions in a variable, called directly, assigned to the field and passed outside New", rel: "internal/engine/wa/x.go", want: 5, src: `package wa

import "context"

var seam = downloadMediaFile

func New() (*Client, error) { return newClient(downloadToFile, downloadMediaFile) }

func newClient(download downloader, fetch mediaDownloader) (*Client, error) {
	c := &Client{download: download, fetch: fetch}
	c.fetch = downloadMediaFile
	return c, nil
}

func (c *Client) FetchMedia(ctx context.Context) error { return downloadMediaFile(ctx, nil, downloadable(""), newCapFile(1, 1)) }

func (c *Client) onReceipt(ctx context.Context) { _ = downloadToFile(ctx, nil, nil, newCapFile(1, 1)) }

func rebuild() (*Client, error) { return newClient(downloadToFile, noFetch) }
`},
		{name: "the seam fields assigned outside newClient", rel: "internal/engine/wa/x.go", want: 3, src: `package wa

func (c *Client) Reset(f mediaDownloader) { c.fetch = f }

func build(f mediaDownloader, d downloader) *Client { return &Client{fetch: f, download: d} }
`},
		{name: "the seam fields invoked outside their methods, as values and in function literals", rel: "internal/engine/wa/x.go", want: 5, src: `package wa

import (
	"context"

	"go.mau.fi/whatsmeow/types/events"
)

func (c *Client) onMessage(ctx context.Context, evt *events.Message, src any) {
	_ = c.fetch(ctx, nil, src, newCapFile(1, 1))
	_ = c.download(ctx, nil, evt.Message.GetProtocolMessage().GetHistorySyncNotification(), newCapFile(1, 1))
}

func (c *Client) FetchMedia(ctx context.Context, src any) error {
	f := c.fetch
	later := func() error { return c.fetch(ctx, nil, src, newCapFile(1, 1)) }
	_, _ = f, later
	return c.download(ctx, nil, nil, newCapFile(1, 1))
}
`},
		{name: "a received message's attachment aimed at either seam", rel: "internal/engine/wa/x.go", want: 4, src: `package wa

import (
	"context"

	"go.mau.fi/whatsmeow/types/events"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/engine/wa/internal/mediasrc"
)

func (c *Client) onMessage(ctx context.Context, evt *events.Message) {
	src := mediasrc.Wrap(evt.Message.GetImageMessage())
	_ = c.fetch(ctx, c.cli, src, newCapFile(1, 1))
	_ = c.download(ctx, c.cli, evt.Message.GetProtocolMessage().GetHistorySyncNotification(), newCapFile(1, 1))
	_, _ = c.FetchMedia(ctx, engine.MediaRequest{})
}
`},
		{name: "FetchMedia called at receipt and taken as a value inside the adapter", rel: "internal/engine/wa/x.go", want: 2, src: `package wa

import (
	"context"

	"github.com/dortort/wawarden/internal/engine"
)

func (c *Client) onReceipt(ctx context.Context) {
	_, _ = c.FetchMedia(ctx, engine.MediaRequest{})
	fetch := c.FetchMedia
	_ = fetch
}
`},
		{name: "a seam handed a parameter, an operating-system file, a reassigned file, a literal or an address-taken file", rel: "internal/engine/wa/x.go", want: 6, src: `package wa

import (
	"context"
	"os"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
)

func (c *Client) FetchMedia(ctx context.Context, f whatsmeow.File, declared int64) error {
	src := downloadable("")
	_ = c.fetch(ctx, nil, src, f)
	tmp, _ := os.CreateTemp("", "x")
	_ = c.fetch(ctx, nil, src, tmp)
	staged := newCapFile(1, 1)
	if declared > 1 {
		staged = reuse()
	}
	_ = c.fetch(ctx, nil, src, staged)
	_ = c.fetch(ctx, nil, src, &memFile{})
	held := newCapFile(1, 1)
	keep(&held)
	_ = c.fetch(ctx, nil, src, held)
	sealed, err := c.spool.Admit(declared, nil)
	if err != nil {
		return err
	}
	_ = c.fetch(ctx, nil, src, sealed)
	return c.fetch(ctx, nil, src, newCapFile(1, 1))
}

func (c *Client) DownloadHistory(ctx context.Context, notif *waE2E.HistorySyncNotification) error {
	return c.download(ctx, nil, notif, os.Stdout)
}
`},
	},
}

type mediaSite struct {
	fn      *ast.FuncDecl
	spec    *ast.TypeSpec
	closure bool
	parent  ast.Node
}

func (s mediaSite) in(name string, method bool) bool {
	return isFunc(s.fn, name, method)
}

func (s mediaSite) direct(name string, method bool) bool {
	return s.in(name, method) && !s.closure
}

func isFunc(fn *ast.FuncDecl, name string, method bool) bool {
	return fn != nil && fn.Name.Name == name && (fn.Recv != nil) == method
}

func spans(outer *ast.FieldList, inner ast.Node) bool {
	return outer != nil && outer.Pos() <= inner.Pos() && inner.End() <= outer.End()
}

func walkMediaSites(file *ast.File, visit func(n ast.Node, s mediaSite)) {
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		var s mediaSite
		for _, p := range stack {
			switch p := p.(type) {
			case *ast.FuncDecl:
				s.fn = p
			case *ast.TypeSpec:
				s.spec = p
			case *ast.FuncLit:
				s.closure = true
			}
		}
		if len(stack) > 0 {
			s.parent = stack[len(stack)-1]
		}
		visit(n, s)
		stack = append(stack, n)
		return true
	})
}

func enclosingFunc(file *ast.File, n ast.Node) *ast.FuncDecl {
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Pos() <= n.Pos() && n.End() <= fd.End() {
			return fd
		}
	}
	return nil
}

func checkMediaDownload(f *sourceFile) []string {
	if f.test {
		return nil
	}
	var out []string
	for _, imp := range f.imports {
		if imp.path != mediasrcPath {
			continue
		}
		if f.dir != adapterDir || f.file.Name.Name != "wa" {
			out = append(out, f.at(imp.node, "only package wa in %s may import %q, whose Source is the one value its media download takes", adapterDir, mediasrcPath))
		}
		if imp.node.Name != nil {
			out = append(out, f.at(imp.node, "the media source package imported as %s: import it under its own name, so every use of Wrap and Source is a selector this rule reads", imp.name))
		}
	}
	if !within(f.dir, adapterDir) {
		return out
	}
	f.compositeLits(func(lit *ast.CompositeLit, typ ast.Expr) {
		out = append(out, f.builtMessage(lit, typ, enclosingFunc(f.file, lit))...)
	})
	walkMediaSites(f.file, func(n ast.Node, s mediaSite) {
		switch e := n.(type) {
		case *ast.CallExpr:
			if id, ok := ast.Unparen(e.Fun).(*ast.Ident); ok && id.Name == "new" && len(e.Args) == 1 {
				out = append(out, f.builtMessage(e, e.Args[0], s.fn)...)
			}
			if sel, ok := ast.Unparen(e.Fun).(*ast.SelectorExpr); ok && seamFields[sel.Sel.Name] != "" && !stagedFile(e, s.fn) {
				out = append(out, f.at(e, "the file a download seam writes is a *capFile from %s or a *stage.Sealed from %s, assigned nothing else in the calling function: any other file could hold plaintext outside the staging limits", ramStaging, diskStaging))
			}
		case *ast.SelectorExpr:
			out = append(out, f.mediaSelector(e, s)...)
		case *ast.KeyValueExpr:
			if id, ok := e.Key.(*ast.Ident); ok && seamFields[id.Name] != "" && !s.in(clientBuilder, false) {
				out = append(out, f.at(id, "the %s seam field is assigned only in %s, so its value is the one %s passes", id.Name, clientBuilder, clientNew))
			}
		case *ast.Ident:
			out = append(out, f.mediaIdent(e, s)...)
		}
	})
	return out
}

func (f *sourceFile) builtMessage(at ast.Node, typ ast.Expr, fn *ast.FuncDecl) []string {
	for _, name := range mediaMessages {
		if f.isType(typ, waE2EPath, name) && !isFunc(fn, mediaBuilder, false) {
			return []string{f.at(at, "a waE2E.%s is built only in %s, from a reference that %s opened: one built anywhere else could carry a received attachment to a download", name, mediaBuilder, mediaFetch)}
		}
	}
	if f.isType(typ, waE2EPath, historyNotification) && !isFunc(fn, historyDownload, true) {
		return []string{f.at(at, "a waE2E.%s is built only in %s, from the history reference the engine checked", historyNotification, historyDownload)}
	}
	return nil
}

func (f *sourceFile) mediaSelector(sel *ast.SelectorExpr, s mediaSite) []string {
	if _, p := f.ref(sel); p == mediasrcPath {
		switch sel.Sel.Name {
		case "Wrap":
			if call, ok := s.parent.(*ast.CallExpr); !ok || call.Fun != sel || !s.direct(mediaBuilder, false) {
				return []string{f.at(sel, "mediasrc.Wrap may only be called directly in the body of %s, outside any function literal: anywhere else, or as a value, it could wrap a received attachment for download", mediaBuilder)}
			}
		case "Source":
			if !sourceMentionAllowed(sel, s) {
				return []string{f.at(sel, "mediasrc.Source may appear in package wa only in the parameters of %s, the result of %s and the parameters of %s: a value declared, built, converted or asserted anywhere else could reach the media download", mediaSeamType, mediaBuilder, mediaSeam)}
			}
		}
		return nil
	}
	if method := seamFields[sel.Sel.Name]; method != "" {
		call, isCall := s.parent.(*ast.CallExpr)
		assign, isAssign := s.parent.(*ast.AssignStmt)
		invoked := isCall && call.Fun == sel && s.direct(method, true)
		assigned := isAssign && slices.Contains(assign.Lhs, ast.Expr(sel)) && s.in(clientBuilder, false)
		if !invoked && !assigned {
			return []string{f.at(sel, "the %s seam field is invoked only directly in %s and assigned only in %s: used anywhere else it could start a download the engine did not ask for", sel.Sel.Name, method, clientBuilder)}
		}
	}
	if sel.Sel.Name == mediaFetch {
		return []string{f.at(sel, "%s is called only by the engine through its client interface: a call or a method value in %s could start a download at receipt", mediaFetch, adapterDir)}
	}
	return nil
}

func sourceMentionAllowed(sel *ast.SelectorExpr, s mediaSite) bool {
	if s.fn == nil && s.spec != nil && s.spec.Name.Name == mediaSeamType {
		ft, ok := s.spec.Type.(*ast.FuncType)
		return ok && spans(ft.Params, sel)
	}
	switch {
	case isFunc(s.fn, mediaBuilder, false):
		return spans(s.fn.Type.Results, sel)
	case isFunc(s.fn, mediaSeam, false):
		return spans(s.fn.Type.Params, sel)
	}
	return false
}

func (f *sourceFile) mediaIdent(id *ast.Ident, s mediaSite) []string {
	switch p := s.parent.(type) {
	case *ast.FuncDecl:
		if p.Name == id {
			return nil
		}
	case *ast.SelectorExpr:
		if p.Sel == id {
			return nil
		}
	}
	call, isCall := s.parent.(*ast.CallExpr)
	switch id.Name {
	case mediaBuilder:
		if !isCall || call.Fun != id || !s.direct(mediaFetch, true) {
			return []string{f.at(id, "%s is referenced only by a direct call in %s, outside any function literal: anywhere else it could build a download from a received message", mediaBuilder, mediaFetch)}
		}
	case mediaSeam, historySeam:
		if !isCall || !isIdent(call.Fun, clientBuilder) || !slices.Contains(call.Args, ast.Expr(id)) || !s.direct(clientNew, false) {
			return []string{f.at(id, "%s is referenced only as an argument to %s in %s: called or stored anywhere else, it would download without the seam the client holds", id.Name, clientBuilder, clientNew)}
		}
	}
	return nil
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := ast.Unparen(e).(*ast.Ident)
	return ok && id.Name == name
}

func stagedFile(call *ast.CallExpr, fn *ast.FuncDecl) bool {
	if len(call.Args) != 4 {
		return false
	}
	arg := ast.Unparen(call.Args[3])
	if c, ok := arg.(*ast.CallExpr); ok && isIdent(c.Fun, ramStaging) {
		return true
	}
	id, ok := arg.(*ast.Ident)
	if !ok || fn == nil || fn.Body == nil {
		return false
	}
	assigned, only := false, true
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				if isIdent(lhs, id.Name) {
					assigned, only = true, only && stagingValue(s.Rhs, len(s.Lhs), i)
				}
			}
		case *ast.ValueSpec:
			for i, name := range s.Names {
				if name.Name == id.Name && len(s.Values) > 0 {
					assigned, only = true, only && stagingValue(s.Values, len(s.Names), i)
				}
			}
		case *ast.RangeStmt:
			only = only && !isIdent(s.Key, id.Name) && !isIdent(s.Value, id.Name)
		case *ast.UnaryExpr:
			only = only && (s.Op != token.AND || !isIdent(s.X, id.Name))
		}
		return true
	})
	return assigned && only
}

func stagingValue(values []ast.Expr, names, i int) bool {
	var v ast.Expr
	switch {
	case len(values) == names:
		v = values[i]
	case len(values) == 1 && i == 0:
		v = values[0]
	default:
		return false
	}
	call, ok := ast.Unparen(v).(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		return fun.Name == ramStaging && len(values) == names
	case *ast.SelectorExpr:
		return fun.Sel.Name == diskStaging && len(values) == 1
	}
	return false
}

func mediaSourceExports(files []*sourceFile) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		if f.test || f.dir != "." {
			continue
		}
		for _, decl := range f.file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.IsExported() {
					out[receiverPrefix(d)+d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if !s.Name.IsExported() {
							continue
						}
						out[s.Name.Name] = true
						if st, ok := s.Type.(*ast.StructType); ok {
							for _, field := range st.Fields.List {
								for _, name := range fieldNames(field) {
									out[s.Name.Name+"."+name] = true
								}
							}
						}
					case *ast.ValueSpec:
						for _, name := range s.Names {
							if name.IsExported() {
								out[name.Name] = true
							}
						}
					}
				}
			}
		}
	}
	return out
}

func fieldNames(field *ast.Field) []string {
	if len(field.Names) == 0 {
		return []string{embeddedName(field.Type)}
	}
	names := make([]string, 0, len(field.Names))
	for _, n := range field.Names {
		names = append(names, n.Name)
	}
	return names
}

func embeddedName(e ast.Expr) string {
	switch t := ast.Unparen(e).(type) {
	case *ast.StarExpr:
		return embeddedName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.Ident:
		return t.Name
	}
	return "embedded"
}

func TestMediaSourceExportsOnlyItsWrapper(t *testing.T) {
	files, err := moduleFiles(os.DirFS(filepath.Join(moduleRoot(t), mediasrcDir)))
	if err != nil {
		t.Fatalf("walk %s: %v", mediasrcDir, err)
	}
	if got, want := slices.Sorted(maps.Keys(mediaSourceExports(files))), slices.Sorted(maps.Keys(mediaSourceSurface)); !slices.Equal(got, want) {
		t.Fatalf("package mediasrc declares %q, but %q were reviewed: Source keeps one unexported field and only Wrap fills it, so review any change and update the list", got, want)
	}
}

func TestMediaSourceInventory(t *testing.T) {
	f, err := parseSource("source.go", []byte(`package mediasrc

import "go.mau.fi/whatsmeow"

type Source struct {
	m    whatsmeow.DownloadableMessage
	Open bool
	*whatsmeow.Client
}

type hidden struct{ Exported int }

type Alias = Source

const Version = 1

var (
	Forge = Wrap
	spare = 0
)

func Wrap(m whatsmeow.DownloadableMessage) Source   { return Source{m: m} }
func (s Source) Message() (whatsmeow.DownloadableMessage, bool) { return s.m, s.m != nil }
func (s *Source) Set(m whatsmeow.DownloadableMessage) { s.m = m }
func (h hidden) Leak() Source                        { return Source{} }
func helper() Source                                  { return Source{} }
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := set("Source", "Source.m", "Source.Open", "Source.Client", "Alias", "Version", "Forge", "Wrap", "Source.Message", "Source.Set", "hidden.Leak")
	if got := mediaSourceExports([]*sourceFile{f}); !maps.Equal(got, want) {
		t.Errorf("inventory %q, want %q", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
}
