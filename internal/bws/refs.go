package bws

// IsUUIDRef reports whether s has the shape of a UUID: 36 characters with
// hyphens at positions 8/13/18/23 and hex digits (in BOTH cases —
// uuid.Parse parity) elsewhere. It routes bws:// refs — a uuid ref is
// fetched by id directly, anything else is treated as a secret name — and
// shape-checks org ids before path interpolation.
//
// shortcut: a secret NAMED like a UUID is unreachable by name (its ref is
// routed to the by-id GET); upgrade when namespaced refs exist.
func IsUUIDRef(s string) bool {
	if len(s) != 36 {
		return false
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			continue
		}
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
