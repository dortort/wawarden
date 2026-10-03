package policy

import "maps"

type ReadGrant struct {
	client string
	all    bool
	chats  map[CanonicalChat]struct{}
	ok     bool
}

func (g ReadGrant) Client() string { return g.client }
func (g ReadGrant) Valid() bool    { return g.ok }
func (g ReadGrant) All() bool      { return g.ok && g.all }

func (g ReadGrant) Chats() map[CanonicalChat]struct{} {
	if !g.ok {
		return nil
	}
	return maps.Clone(g.chats)
}

func (g ReadGrant) Allows(chat CanonicalChat) bool {
	if !g.ok || !chat.ok {
		return false
	}
	if g.all {
		return true
	}
	_, ok := g.chats[chat]
	return ok
}

type WriteGrant struct {
	client            string
	chats             map[CanonicalChat]struct{}
	allowFirstContact bool
	ok                bool
}

func (g WriteGrant) Client() string          { return g.client }
func (g WriteGrant) Valid() bool             { return g.ok }
func (g WriteGrant) AllowFirstContact() bool { return g.ok && g.allowFirstContact }

func (g WriteGrant) Allows(chat CanonicalChat) bool {
	if !g.ok || !chat.ok {
		return false
	}
	_, ok := g.chats[chat]
	return ok
}

type AdminGrant struct {
	ok bool
}

func (g AdminGrant) Valid() bool { return g.ok }
