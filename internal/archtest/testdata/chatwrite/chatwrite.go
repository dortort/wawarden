package policy

import "unique"

func forge(c *CanonicalChat, jid string) { c.jid = unique.Make(jid) }
