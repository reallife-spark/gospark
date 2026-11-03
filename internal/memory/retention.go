package memory

import "time"

// Default retention windows. Semantic memory follows the vault's own lifecycle
// and is therefore unbounded here; short-term memory never outlives the
// process and has no persisted deadline.
var defaultRetentionDays = map[Type]int{
	Episodic:   90,
	Semantic:   0,
	Procedural: 365,
	ShortTerm:  0,
	Shared:     30,
}

// RetentionPolicy is the effective window for one record type.
type RetentionPolicy map[Type]int

// DefaultRetentionPolicy returns the fabric defaults.
func DefaultRetentionPolicy() RetentionPolicy {
	out := make(RetentionPolicy, len(defaultRetentionDays))
	for t, days := range defaultRetentionDays {
		out[t] = days
	}
	return out
}

// WithOverrides returns a copy with per-type day overrides. A zero value means
// "no expiry" and is a deliberate override, not a missing entry.
func (p RetentionPolicy) WithOverrides(overrides map[Type]int) RetentionPolicy {
	out := make(RetentionPolicy, len(p)+len(overrides))
	for t, days := range p {
		out[t] = days
	}
	for t, days := range overrides {
		if days < 0 {
			days = 0
		}
		out[t] = days
	}
	return out
}

// Until returns the retention deadline for a type at a given time.
func (p RetentionPolicy) Until(t Type, now time.Time) string {
	days := p[t]
	if days <= 0 {
		return ""
	}
	return now.UTC().AddDate(0, 0, days).Format(time.RFC3339)
}

func defaultRetention(t Type, now time.Time) string {
	return DefaultRetentionPolicy().Until(t, now)
}
