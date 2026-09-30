package event

// Less orders scoring history by event time, authenticated producer, then id.
// Producer-less offline fixtures use the empty producer namespace.
func Less(a, b Event) bool {
	if !a.At.Equal(b.At) {
		return a.At.Before(b.At)
	}
	if a.Producer != b.Producer {
		return a.Producer < b.Producer
	}
	return a.ID < b.ID
}
