// Package mediasrc declares the attachment source the adapter's media download takes: only Wrap puts a message in one, and the zero value carries none.
package mediasrc

import "go.mau.fi/whatsmeow"

type Source struct {
	m whatsmeow.DownloadableMessage
}

func Wrap(m whatsmeow.DownloadableMessage) Source {
	return Source{m: m}
}

func (s Source) Message() (whatsmeow.DownloadableMessage, bool) {
	return s.m, s.m != nil
}
