package bws

import "testing"

// TestIsUUIDRef pins the uuid.Parse-parity classifier: 36 chars, hyphens at
// 8/13/18/23, hex digits accepted in BOTH cases. It is used to route
// bws:// refs (uuid -> GET by id, name -> org list) and to shape-check org
// ids before path interpolation.
func TestIsUUIDRef(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want bool
	}{
		{"canonical lowercase", "01234567-89ab-cdef-0123-456789abcdef", true},
		{"uppercase hex", "01234567-89AB-CDEF-0123-456789ABCDEF", true},
		{"mixed case hex", "01234567-89aB-cDeF-0123-456789aBcDeF", true},
		{"non-hex letter", "01234567-89ab-cgeg-0123-456789abcdef", false},
		{"nil byte", "01234567089ab-cdef-0123-456789abcdef", false},
		{"35 chars", "1234567-89ab-cdef-0123-456789abcdef", false},
		{"37 chars", "012345671-89ab-cdef-0123-456789abcdef", false},
		{"missing hyphen", "0123456789ab-cdef-0123-456789abcdef", false},
		{"hyphen wrong position", "01234567-89ab-cdef-0123-456789abcde-", false},
		{"all hyphens missing", "0123456789abcdef0123456789abcdef", false},
		{"empty", "", false},
		{"name ref", "jev", false},
		{"name ref with dots", "my.secret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUUIDRef(tc.ref); got != tc.want {
				t.Errorf("IsUUIDRef(%q) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}
