package ratelimit

func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.due)
}

func (h *HourLog) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.entries)
}
