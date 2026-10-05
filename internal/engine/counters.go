package engine

import "github.com/dortort/wawarden/internal/metrics"

const (
	dropChat         = "chat_rejected"
	dropSender       = "sender_rejected"
	dropInvalid      = "invalid"
	dropUnknownKind  = "unknown_kind"
	dropNoTarget     = "no_target"
	dropForeign      = "foreign_reference"
	dropTargetAbsent = "target_unknown"
	dropTargetKind   = "target_kind"
	dropStaleEdit    = "stale_edit"
	dropNotOriginal  = "not_original_sender"
	dropNotAdmin     = "not_admin"
	dropAdminUnknown = "admin_unknown"
	dropOwnerUnknown = "owner_unknown"

	refusedBacklog = "backlog_full"
	refusedPaused  = "paused"
	refusedStore   = "store_error"

	queueInbox = "inbox"
)

type counters struct {
	ingested    *metrics.Counter
	dropped     *metrics.CounterVec
	refused     *metrics.CounterVec
	quarantined *metrics.CounterVec
	conflicts   *metrics.CounterVec
}

func newCounters(reg *metrics.Registry) *counters {
	return &counters{
		ingested:    reg.Counter("wawarden_messages_ingested_total", "Messages, reactions and poll updates stored in the archive, live and from history."),
		dropped:     reg.CounterVec("wawarden_ingest_dropped_total", "WhatsApp events dropped by an ingest rule, by reason.", "reason"),
		refused:     reg.CounterVec("wawarden_ingest_refused_total", "WhatsApp events left unacknowledged so that WhatsApp delivers them again, by reason.", "reason"),
		quarantined: reg.CounterVec("wawarden_ingest_quarantined_total", "Inbox rows and history blobs set aside after three failed attempts, by queue.", "queue"),
		conflicts:   reg.CounterVec("wawarden_rekey_conflicts_total", "Identity mappings refused because they contradict the archive, by conflict.", "conflict"),
	}
}
