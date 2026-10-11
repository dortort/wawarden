package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestARejectionCarriesTheServersNumber(t *testing.T) {
	for _, tt := range []struct {
		err  error
		code int
		text string
	}{
		{&RejectedError{Code: 479}, 479, "engine: WhatsApp rejected the message with code 479"},
		{fmt.Errorf("wrapped: %w", &RejectedError{Code: 463}), 463, "wrapped: engine: WhatsApp rejected the message with code 463"},
		{&RejectedError{}, 0, "engine: WhatsApp rejected the message"},
	} {
		if !errors.Is(tt.err, ErrRejected) || RejectionCode(tt.err) != tt.code || tt.err.Error() != tt.text {
			t.Errorf("%v: Is(ErrRejected) %v, code %d", tt.err, errors.Is(tt.err, ErrRejected), RejectionCode(tt.err))
		}
	}
	for _, err := range []error{nil, ErrRejected, ErrOutcomeUnknown} {
		if RejectionCode(err) != 0 {
			t.Errorf("RejectionCode(%v) = %d", err, RejectionCode(err))
		}
	}
	for _, err := range []error{ErrNotConnected, ErrRecipientUnavailable, ErrOutcomeUnknown, ErrSendBusy} {
		if errors.Is(err, ErrRejected) {
			t.Errorf("%v reads as a rejection", err)
		}
	}
}

func TestHistoryRefsKeepTheirStoredFormWithoutAnOnDemandAnswer(t *testing.T) {
	plain, err := json.Marshal(HistoryRef{ID: "HS1", DirectPath: "/synthetic"})
	if err != nil || string(plain) != `{"id":"HS1","direct_path":"/synthetic"}` {
		t.Fatalf("a plain ref encodes as %s, %v", plain, err)
	}
	answer := HistoryRef{ID: "HS2", DirectPath: "/synthetic", SyncType: SyncOnDemand, RequestID: "3EB0SYNTHETIC"}
	encoded, err := json.Marshal(answer)
	if err != nil || string(encoded) != `{"id":"HS2","direct_path":"/synthetic","sync_type":"ON_DEMAND","request_id":"3EB0SYNTHETIC"}` {
		t.Fatalf("an on-demand ref encodes as %s, %v", encoded, err)
	}
	var back HistoryRef
	if err := json.Unmarshal(encoded, &back); err != nil || back.SyncType != SyncOnDemand || back.RequestID != answer.RequestID {
		t.Fatalf("decoded %+v, %v", back, err)
	}
}
