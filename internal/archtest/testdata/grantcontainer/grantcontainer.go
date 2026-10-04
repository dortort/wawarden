package policy

type list[T any] []T

var forged = list[WriteGrant]{{ok: true}}
