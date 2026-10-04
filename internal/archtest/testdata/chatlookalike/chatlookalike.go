package policy

import "unique"

type lookalike struct {
	jid  unique.Handle[string]
	kind ChatKind
	ok   bool
}

var forged = CanonicalChat(lookalike{jid: unique.Make("15550100001@s.whatsapp.net"), kind: PhoneChat, ok: true})
