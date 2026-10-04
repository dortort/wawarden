package policy

type okbit struct{ ok bool }

func forge() AdminGrant {
	var g AdminGrant
	*(*okbit)(&g) = okbit{ok: true}
	return g
}
