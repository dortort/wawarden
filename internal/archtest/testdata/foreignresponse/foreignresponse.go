package foreignresponse

import "github.com/dortort/wawarden/internal/api/dto"

type leak struct{}

func (leak) encode() (string, []byte, error) { return "", nil, nil }

func Respond() dto.Response { return leak{} }
