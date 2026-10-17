//go:build dev

// Package fake is a scripted engine client for local development, built only with the dev tag: it pairs, connects and delivers synthetic events and history without any network.
package fake

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/safego"
)

const (
	pairingCode     = "FAKE-C0DE"
	defaultInterval = 100 * time.Millisecond
	wrongAccount    = "15550100029"
	maxBackfill     = 50

	fakeMarker = "wawarden-fake-engine-not-for-release"
)

var marker string

// Assigned in init because the linker drops unreferenced constants and statically initialised variables.
func init() { marker = fakeMarker }

var (
	errOptions     = errors.New("fake: the history cap must be from 1 byte to 256 MiB, and the owner's phone number, when set, must be E.164 and not one of the fake's synthetic numbers")
	errNotOwner    = errors.New("fake: pairing is offered to the owner's number only")
	errOffline     = errors.New("fake: pairing needs a connection")
	errUnknownBlob = errors.New("fake: no such history blob")
)

var version = engine.Version{2, 3000, 1}

type Options struct {
	OwnerPhone      string
	WrongAccount    bool
	HistoryMaxBytes int64
	Interval        time.Duration
}

type Client struct {
	owner    string
	account  string
	max      int64
	interval time.Duration
	script   []engine.Event
	recent   []byte
	poison   []byte
	stamp    string

	mu        sync.Mutex
	handler   func(engine.Event) bool
	paired    bool
	connected bool
	version   engine.Version
	conn      context.Context
	cancel    context.CancelFunc
	pair      chan struct{}
	next      int
	ids       int
	sent      []engine.SendRequest
	answers   map[string]engine.BackfillRequest
}

func New(opts Options) (*Client, error) {
	owner, ok := policy.OwnerDigits(opts.OwnerPhone)
	if opts.OwnerPhone != "" && (!ok || slices.Contains(synthetic, owner)) || opts.HistoryMaxBytes <= 0 || opts.HistoryMaxBytes > engine.MaxHistoryMaxBytes {
		return nil, errOptions
	}
	now := time.Now()
	c := &Client{owner: owner, account: owner, max: opts.HistoryMaxBytes, interval: opts.Interval, version: version,
		stamp: now.UTC().Format("20060102-150405") + "-" + decimal(now.Nanosecond()), answers: map[string]engine.BackfillRequest{}}
	if opts.WrongAccount {
		c.account = wrongAccount
	}
	if c.interval <= 0 {
		c.interval = defaultInterval
	}
	bootstrap, err := compress(bootstrapHistory(owner, now))
	if err != nil {
		return nil, err
	}
	if c.recent, err = compress(recentHistory(owner, now)); err != nil {
		return nil, err
	}
	if c.poison, err = deflate([]byte("synthetic poison: this blob is not in the fake's history encoding")); err != nil {
		return nil, err
	}
	c.script = script(owner, bootstrap)
	return c, nil
}

func compress(h engine.History) ([]byte, error) {
	raw, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	return deflate(raw)
}

func deflate(raw []byte) ([]byte, error) {
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func (c *Client) Connect(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, cancel := context.WithCancel(context.Background())
	pair := make(chan struct{}, 1)
	c.mu.Lock()
	c.hangUpLocked()
	c.conn, c.cancel, c.pair = conn, cancel, pair
	c.mu.Unlock()
	safego.Go("fake.connection", func() { c.run(conn, pair) })
	return nil
}

func (c *Client) Disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hangUpLocked()
}

func (c *Client) hangUpLocked() {
	if c.cancel != nil {
		c.cancel()
	}
	c.conn, c.cancel, c.pair, c.connected = nil, nil, nil, false
}

func (c *Client) Paired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paired
}

func (c *Client) Account() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paired {
		return ""
	}
	return c.account
}

func (c *Client) PairPhone(ctx context.Context, digits string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.owner == "" || digits != c.owner {
		return "", errNotOwner
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pair == nil {
		return "", errOffline
	}
	select {
	case c.pair <- struct{}{}:
	default:
	}
	return pairingCode, nil
}

func (c *Client) Logout(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paired = false
	c.hangUpLocked()
	return nil
}

func (c *Client) Version() engine.Version {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

func (c *Client) SetVersion(v engine.Version) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.version = v
}

func (c *Client) Latest(context.Context) (engine.Version, error) { return version, nil }

func (c *Client) DownloadHistory(ctx context.Context, ref engine.HistoryRef, dst io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch ref.ID {
	case recentID:
		_, err := dst.Write(c.recent)
		return err
	case poisonID:
		_, err := dst.Write(c.poison)
		return err
	case oversizedID:
		return inflatingPast(ctx, dst, c.max)
	}
	c.mu.Lock()
	req, ok := c.answers[ref.ID]
	c.mu.Unlock()
	if !ok {
		return errUnknownBlob
	}
	blob, err := compress(olderHistory(ref.ID, req))
	if err != nil {
		return err
	}
	_, err = dst.Write(blob)
	return err
}

func inflatingPast(ctx context.Context, dst io.Writer, limit int64) error {
	zw := zlib.NewWriter(dst)
	zeros := make([]byte, 64<<10)
	for left := limit + 1; left > 0; left -= int64(len(zeros)) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := zw.Write(zeros[:min(left, int64(len(zeros)))]); err != nil {
			return err
		}
	}
	return zw.Close()
}

func (c *Client) AckHistory(context.Context, engine.HistoryRef) error { return nil }

func (c *Client) Decode(blob []byte) (engine.History, error) {
	var h engine.History
	if err := json.Unmarshal(blob, &h); err != nil {
		return engine.History{}, err
	}
	return h, nil
}

func (c *Client) OnEvent(handler func(engine.Event) bool) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handler = handler
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.handler = nil
	}
}

func (c *Client) deliver(ev engine.Event) bool {
	c.mu.Lock()
	handler := c.handler
	c.mu.Unlock()
	return handler != nil && handler(ev)
}

func (c *Client) wait(ctx context.Context) bool {
	t := time.NewTimer(c.interval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (c *Client) run(ctx context.Context, pair <-chan struct{}) {
	if !c.Paired() {
		select {
		case <-ctx.Done():
			return
		case <-pair:
		}
		if !c.wait(ctx) || !c.link(ctx) {
			return
		}
	}
	if c.Account() == c.owner {
		c.online(ctx)
	}
	c.deliver(engine.Connected{})
	if c.Account() != c.owner {
		return
	}
	for c.wait(ctx) {
		ev, step, ok := c.peek()
		if !ok {
			return
		}
		if _, drop := ev.(engine.Disconnected); drop {
			if c.drop(ctx, step) {
				c.deliver(ev)
			}
			return
		}
		if c.deliver(stamped(ev, time.Now())) {
			c.advance(step)
		}
	}
}

func (c *Client) online(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = ctx.Err() == nil
}

func (c *Client) link(ctx context.Context) bool {
	c.mu.Lock()
	if ctx.Err() != nil {
		c.mu.Unlock()
		return false
	}
	c.paired = true
	jid := c.account + ":" + linkedDevice + server
	c.mu.Unlock()
	c.deliver(engine.Paired{JID: jid})
	return true
}

func (c *Client) peek() (engine.Event, int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.next >= len(c.script) {
		return nil, 0, false
	}
	return c.script[c.next], c.next, true
}

func (c *Client) advance(step int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.next == step {
		c.next++
	}
}

func (c *Client) drop(ctx context.Context, step int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	if c.next == step {
		c.next++
	}
	c.hangUpLocked()
	return true
}

func stamped(ev engine.Event, now time.Time) engine.Event {
	switch e := ev.(type) {
	case engine.Message:
		e.Timestamp = now
		return e
	case engine.Group:
		e.Timestamp = now
		return e
	}
	return ev
}

func (c *Client) NewMessageID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids++
	return "FAKE-S" + c.stamp + "-" + decimal(c.ids)
}

func (c *Client) Send(ctx context.Context, req engine.SendRequest) (engine.Sent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() != nil || !c.connected {
		return engine.Sent{}, engine.ErrNotConnected
	}
	if req.ID == "" || req.Text == "" {
		return engine.Sent{}, &engine.RejectedError{}
	}
	switch req.Chat {
	case alice, bob, aliceLID, bobLID:
	case unreachable:
		c.sent = append(c.sent, req)
		return engine.Sent{}, engine.ErrOutcomeUnknown
	default:
		return engine.Sent{}, engine.ErrRecipientUnavailable
	}
	c.sent = append(c.sent, req)
	sender := c.owner + server
	if req.Chat == aliceLID || req.Chat == bobLID {
		sender = ownerLID
	}
	return engine.Sent{ID: req.ID, Chat: req.Chat, Sender: sender, Timestamp: time.Now()}, nil
}

func (c *Client) Sent() []engine.SendRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sent)
}

func (c *Client) Backfill(ctx context.Context, req engine.BackfillRequest) (string, error) {
	c.mu.Lock()
	if ctx.Err() != nil || !c.connected {
		c.mu.Unlock()
		return "", engine.ErrNotConnected
	}
	if req.AnchorID == "" || req.AnchorTime.IsZero() || req.Count < 1 || req.Count > maxBackfill || !slices.Contains(chats, req.Chat) {
		c.mu.Unlock()
		return "", &engine.RejectedError{}
	}
	c.ids++
	n := c.stamp + "-" + decimal(c.ids)
	id, blob := "FAKE-BF"+n, onDemandID+n
	c.answers[blob] = req
	conn, sender := c.conn, c.owner+server
	c.mu.Unlock()
	answer := engine.HistoryNotification{Sender: sender, FromMe: true, Ref: engine.HistoryRef{ID: blob, DirectPath: "/fake/history/ondemand", SyncType: engine.SyncOnDemand, RequestID: id}}
	safego.Go("fake.backfill", func() {
		for c.wait(conn) && !c.deliver(answer) {
		}
	})
	return id, nil
}

func olderHistory(blob string, req engine.BackfillRequest) engine.History {
	sender := req.Chat
	if req.Chat == group {
		sender = alice
	}
	messages := make([]engine.Message, 0, req.Count)
	for i := range req.Count {
		messages = append(messages, engine.Message{
			ID: blob + "-" + decimal(i+1), Sender: sender, Timestamp: req.AnchorTime.Add(-time.Duration(i+1) * time.Minute),
			Kind: engine.KindText, Text: "An older synthetic message, from the on-demand history",
		})
	}
	return engine.History{Conversations: []engine.Conversation{{Chat: req.Chat, Messages: messages}}}
}

func decimal(n int) string {
	digits := []byte{"0123456789"[n%10]}
	for n /= 10; n > 0; n /= 10 {
		digits = append(digits, "0123456789"[n%10])
	}
	slices.Reverse(digits)
	return string(digits)
}
