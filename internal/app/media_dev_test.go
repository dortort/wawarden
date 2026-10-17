//go:build dev

package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/token"
)

const (
	fakeChat        = "15550100021@s.whatsapp.net"
	fakeLargeSize   = 8<<20 + 64<<10
	fakeNearCapSize = 8<<20 - 64<<10
	fakePartSize    = 8 << 20
	attachmentURI   = "wawarden:attachment"
	partURI         = "wawarden:attachment-part"
	octetStream     = "application/octet-stream"
	fakeAttachment  = 9
)

type bearerTransport struct {
	token string
	base  *http.Transport
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

type mcpAttachment struct {
	info  dto.AttachmentInfo
	err   string
	kind  string
	mime  string
	uri   string
	data  []byte
	block bool
}

type fakeDownloads struct {
	t      *testing.T
	to     netip.AddrPort
	admin  netip.AddrPort
	token  string
	auth   http.Header
	cs     *mcp.ClientSession
	mrefs  []string
	served map[string]int64
}

func (f *fakeDownloads) status() (counts map[string]float64, downloads string) {
	f.t.Helper()
	r := mustDo(f.t, http.MethodGet, f.admin, "/admin/v1/status", bearer(f.token))
	var s struct {
		Counts    map[string]float64 `json:"counts"`
		Downloads string             `json:"downloads"`
	}
	if r.status != http.StatusOK || json.Unmarshal([]byte(r.body), &s) != nil {
		f.t.Fatalf("status = %d %s", r.status, r.body)
	}
	return s.Counts, s.Downloads
}

func (f *fakeDownloads) metric(series string) string {
	f.t.Helper()
	for line := range strings.SplitSeq(mustDo(f.t, http.MethodGet, f.admin, "/metrics", bearer(f.token)).body, "\n") {
		if value, ok := strings.CutPrefix(line, series+" "); ok {
			return value
		}
	}
	f.t.Fatalf("/metrics lacks %s", series)
	return ""
}

func (f *fakeDownloads) rest(i int) reply {
	f.t.Helper()
	path := "/v1/messages/" + f.mrefs[i] + "/media"
	if r := mustDo(f.t, http.MethodPost, f.to, path, f.auth); r.status != http.StatusAccepted {
		f.t.Fatalf("POST attachment %d = %d %s", i, r.status, r.body)
	}
	var got reply
	waitUntil(f.t, "the attachment job", func() bool {
		got = mustDo(f.t, http.MethodGet, f.to, path, f.auth)
		return got.status != http.StatusAccepted
	})
	if got.status == http.StatusOK {
		f.served["rest"] += int64(len(got.body))
	}
	return got
}

func (f *fakeDownloads) call(i int, part *int) mcpAttachment {
	f.t.Helper()
	args := map[string]any{"message": f.mrefs[i]}
	if part != nil {
		args["part"] = *part
	}
	res, err := f.cs.CallTool(f.t.Context(), &mcp.CallToolParams{Name: "get_attachment", Arguments: args})
	if err != nil {
		f.t.Fatalf("get_attachment %d: %v", i, err)
	}
	structured, err := json.Marshal(res.StructuredContent)
	var a mcpAttachment
	if err != nil || json.Unmarshal(structured, &a.info) != nil {
		f.t.Fatalf("get_attachment %d answered %s, %v", i, structured, err)
	}
	if res.IsError {
		if text, ok := res.Content[0].(*mcp.TextContent); ok && len(res.Content) == 1 {
			a.err = text.Text
		}
		return a
	}
	for _, c := range res.Content {
		switch b := c.(type) {
		case *mcp.ImageContent:
			a.kind, a.mime, a.data, a.block = "image", b.MIMEType, b.Data, true
		case *mcp.AudioContent:
			a.kind, a.mime, a.data, a.block = "audio", b.MIMEType, b.Data, true
		case *mcp.EmbeddedResource:
			a.kind, a.mime, a.uri, a.data, a.block = "blob", b.Resource.MIMEType, b.Resource.URI, b.Resource.Blob, true
		}
	}
	f.served["mcp"] += int64(len(a.data))
	return a
}

func (f *fakeDownloads) inline(i int, kind, mime string, want []byte) mcpAttachment {
	f.t.Helper()
	a := f.call(i, nil)
	if a.err != "" || a.info.Delivery != "inline" || a.kind != kind || a.mime != mime || !bytes.Equal(a.data, want) || a.kind == "blob" && a.uri != attachmentURI {
		f.t.Fatalf("get_attachment %d = %s %s %s %q with %d bytes and %+v, want the REST body as %s %s", i, a.err, a.kind, a.mime, a.uri, len(a.data), a.info, kind, mime)
	}
	return a
}

func (f *fakeDownloads) parts(i int, served string, whole []byte) {
	f.t.Helper()
	fetched := f.metric(`wawarden_media_fetches_total{result="ok"}`)
	if a := f.call(i, nil); a.err != "" || a.block || a.info.Delivery != "part" || a.info.Reason == nil || *a.info.Reason != "too_large_for_inline" ||
		a.info.Parts != 2 || a.info.SizeDeclared != fakeLargeSize {
		f.t.Fatalf("get_attachment %d without a part = %s %+v, want the metadata that points at two parts", i, a.err, a.info)
	}
	if got := f.metric(`wawarden_media_fetches_total{result="ok"}`); got != fetched {
		f.t.Fatalf("the metadata answer fetched: %s fetches, then %s", fetched, got)
	}
	var joined []byte
	for _, k := range []int{0, 2, 1} {
		a := f.call(i, &k)
		if k == 2 {
			if a.err != "range_not_satisfiable" {
				f.t.Fatalf("part 2 of attachment %d = %s %+v, want range_not_satisfiable", i, a.err, a.info)
			}
			continue
		}
		offset, length := int64(k)*fakePartSize, min(fakePartSize, int64(fakeLargeSize-k*fakePartSize))
		if a.err != "" || a.info.Delivery != "part" || a.info.ServedType != served || a.info.Offset != offset || a.info.Length != length || a.kind != "blob" ||
			a.uri != partURI || a.mime != octetStream || int64(len(a.data)) != length {
			f.t.Fatalf("part %d of attachment %d = %s %+v with %s %q and %d bytes", k, i, a.err, a.info, a.mime, a.uri, len(a.data))
		}
		joined = append(joined, a.data...)
	}
	if !bytes.Equal(joined, whole) {
		f.t.Fatalf("the two parts of attachment %d do not join to the REST body", i)
	}
}

func fakeMediaRefs(t *testing.T, to netip.AddrPort, auth http.Header) []string {
	t.Helper()
	listed := slices.DeleteFunc(onlyChatMessages(t, to, auth), func(m listedMessage) bool { return m.Kind != "media" })
	slices.SortFunc(listed, func(a, b listedMessage) int { return strings.Compare(a.TS, b.TS) })
	var refs []string
	for _, m := range listed {
		refs = append(refs, m.Ref)
	}
	if len(refs) != fakeAttachment {
		t.Fatalf("the direct chat holds %d attachments, want %d", len(refs), fakeAttachment)
	}
	return refs
}

func TestTheFakeEngineServesEveryAttachmentKindOverRESTAndMCP(t *testing.T) {
	adminToken := token.NewAdmin()
	cfg := testConfig(t, adminToken)
	cfg.OwnerPhone, cfg.Dev.FakeEngine = "+15550100009", true
	cfg.MediaMaxBytes, cfg.MediaRefRetentionDays = 16<<20, config.DefaultMediaRefRetentionDays
	cfg.MediaPerMinute, cfg.MediaGlobalPerHour, cfg.MediaGlobalBytesPerHour = 60000, config.MaxMediaGlobalPerHour, config.DefaultMediaGlobalBytesPerHour
	logs := &syncBuffer{}
	a, err := New(t.Context(), cfg, logx.NewWriter(logs), testMaster(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stop := run(t, a)
	logs.waitFor(t, "ready")
	f := &fakeDownloads{t: t, to: addr(t, a, listenerClient), admin: addr(t, a, listenerAdmin), token: adminToken, served: map[string]int64{}}
	if r := post(t, f.admin, "/admin/v1/pair", bearer(adminToken), "{}"); r.status != http.StatusOK {
		t.Fatalf("pair = %d %s", r.status, r.body)
	}
	waitUntil(t, "every scripted attachment reference", func() bool { counts, _ := f.status(); return counts["media_refs_live"] == fakeAttachment })
	if counts, downloads := f.status(); counts["media_refs_expired"] != 0 || downloads != "on" {
		t.Fatalf("status counts %v and downloads %q after the script", counts, downloads)
	}
	created := post(t, f.admin, "/admin/v1/clients", bearer(adminToken), `{"name":"synthetic attachment reader","read_chats":["`+fakeChat+`"],"allow_media":true}`)
	var client struct {
		Credential string `json:"credential"`
	}
	if created.status != http.StatusOK || json.Unmarshal([]byte(created.body), &client) != nil || client.Credential == "" {
		t.Fatalf("create = %d %s", created.status, created.body)
	}
	f.auth = bearer(client.Credential)
	f.mrefs = fakeMediaRefs(t, f.to, f.auth)
	base := &http.Transport{}
	t.Cleanup(base.CloseIdleConnections)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "wawarden-test", Version: "0"}, nil).Connect(t.Context(),
		&mcp.StreamableClientTransport{Endpoint: "http://" + f.to.String() + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token: client.Credential, base: base}}, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	f.cs = session

	const photo, voice, video, pdf, sticker, jpegDocument, corrupt, expired, nearCap = 0, 1, 2, 3, 4, 5, 6, 7, 8
	served := func(i int, contentType string) reply {
		t.Helper()
		r := f.rest(i)
		if r.status != http.StatusOK || r.header.Get("Content-Type") != contentType {
			t.Fatalf("attachment %d over REST = %d %v with %d bytes, want %s", i, r.status, r.header, len(r.body), contentType)
		}
		return r
	}
	normalisedJPEG := served(photo, "image/jpeg")
	if !strings.HasPrefix(normalisedJPEG.body, "\xff\xd8\xff") {
		t.Fatal("the photo is not served as a JPEG")
	}
	if a := f.inline(photo, "image", "image/jpeg", []byte(normalisedJPEG.body)); !a.info.Normalised || a.info.Format != "image" {
		t.Fatalf("the photo over MCP = %+v", a.info)
	}
	ogg := served(voice, "audio/ogg")
	if !strings.HasPrefix(ogg.body, "OggS") || !strings.Contains(ogg.body, "OpusHead") {
		t.Fatal("the voice note is not served as Ogg Opus")
	}
	f.inline(voice, "audio", "audio/ogg", []byte(ogg.body))
	mp4 := served(video, "video/mp4")
	if len(mp4.body) != fakeLargeSize || mp4.body[4:8] != "ftyp" || strings.Contains(mp4.body, "A synthetic WaWarden video") {
		t.Fatalf("the video over REST holds %d bytes and its metadata %v", len(mp4.body), strings.Contains(mp4.body, "A synthetic WaWarden video"))
	}
	f.parts(video, "video/mp4", []byte(mp4.body))
	document := served(pdf, "application/pdf")
	if len(document.body) != fakeLargeSize || !strings.HasPrefix(document.body, "%PDF-") {
		t.Fatalf("the PDF over REST holds %d bytes", len(document.body))
	}
	f.parts(pdf, "application/pdf", []byte(document.body))
	webp := served(sticker, "image/jpeg")
	f.inline(sticker, "image", "image/jpeg", []byte(webp.body))
	if doc := served(jpegDocument, "image/jpeg"); doc.body != normalisedJPEG.body {
		t.Fatal("the JPEG sent as a document is not served as the normalised photo")
	}
	if a := f.inline(jpegDocument, "image", "image/jpeg", []byte(normalisedJPEG.body)); !a.info.Normalised {
		t.Fatalf("the JPEG document over MCP = %+v", a.info)
	}
	raw := served(corrupt, octetStream)
	if len(raw.body) != 188 || !strings.HasPrefix(raw.body, "\xff\xd8\xff") || raw.header.Get("Content-Disposition") != `attachment; filename="wawarden-document.bin"` {
		t.Fatalf("the refused JPEG document over REST = %v with %d bytes", raw.header, len(raw.body))
	}
	if a := f.inline(corrupt, "blob", octetStream, []byte(raw.body)); a.info.Normalised || a.info.Format != "image" || a.info.ServedType != octetStream {
		t.Fatalf("the refused JPEG document over MCP = %+v", a.info)
	}
	nearCapDocument := served(nearCap, "application/pdf")
	if len(nearCapDocument.body) != fakeNearCapSize || !strings.HasPrefix(nearCapDocument.body, "%PDF-") {
		t.Fatalf("the near-cap PDF over REST holds %d bytes", len(nearCapDocument.body))
	}
	if a := f.inline(nearCap, "blob", "application/pdf", []byte(nearCapDocument.body)); a.info.Bytes != fakeNearCapSize || a.info.Format != "pdf" {
		t.Fatalf("the near-cap PDF over MCP = %+v", a.info)
	}

	expiredPath := "/v1/messages/" + f.mrefs[expired] + "/media"
	if r := mustDo(t, http.MethodPost, f.to, expiredPath, f.auth); r.status != http.StatusAccepted {
		t.Fatalf("POST the expired attachment = %d %s, want 202", r.status, r.body)
	}
	waitUntil(t, "the expired fetch to end", func() bool { return f.metric(`wawarden_media_fetches_total{result="expired"}`) == "1" })
	if r := mustDo(t, http.MethodGet, f.to, expiredPath, f.auth); r.status != http.StatusGone || r.body != `{"error":"media_expired"}` {
		t.Fatalf("the expired attachment over REST = %d %s, want 410 media_expired", r.status, r.body)
	}
	if a := f.call(expired, nil); a.err != "media_unavailable" {
		t.Fatalf("the expired attachment over MCP afterwards = %s, want media_unavailable", a.err)
	}
	if r := mustDo(t, http.MethodPost, f.to, expiredPath, f.auth); r.status != http.StatusNotFound || r.body != `{"error":"media_unavailable"}` {
		t.Fatalf("the expired attachment over REST afterwards = %d %s", r.status, r.body)
	}
	if counts, _ := f.status(); counts["media_refs_live"] != fakeAttachment-1 || counts["media_refs_expired"] != 1 {
		t.Fatalf("status counts %v after the expiry, want eight live references and one expired", counts)
	}

	for series, want := range map[string]string{
		`wawarden_media_fetches_total{result="ok"}`:                     "16",
		`wawarden_media_fetches_total{result="expired"}`:                "1",
		`wawarden_media_fetches_total{result="rate_limited"}`:           "0",
		`wawarden_media_fetched_bytes_total`:                            strconv.Itoa(2 * (377 + 120 + fakeLargeSize + fakeLargeSize + 32 + 377 + 188 + fakeNearCapSize)),
		`wawarden_media_deliveries_total{transport="rest",result="ok"}`: "8",
		`wawarden_media_deliveries_total{transport="mcp",result="ok"}`:  "10",
		`wawarden_media_delivered_bytes_total{transport="rest"}`:        strconv.FormatInt(f.served["rest"], 10),
		`wawarden_media_delivered_bytes_total{transport="mcp"}`:         strconv.FormatInt(f.served["mcp"], 10),
		`wawarden_media_jobs`:                                           "0",
	} {
		if got := f.metric(series); got != want {
			t.Errorf("%s = %s, want %s", series, got, want)
		}
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
}
