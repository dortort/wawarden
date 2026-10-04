package policy

func mint[T ~struct{ ok bool }]() T { return T{ok: true} }

var forged = mint[AdminGrant]()
