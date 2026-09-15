package alertsource

import "testing"

// The exception for indistinguishable duplicates must stay an exception. The
// real table has exactly one such pair and no genuine within-raion collision,
// so this reaches the rule directly rather than through the matcher — the
// case that matters cannot be constructed from the shipped data, and a guard
// nothing can exercise is a guard nobody can trust.
func TestOnlyIndistinguishableRefusesCandidatesThatDiffer(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []Location
		want bool
	}{
		{
			// Two rows, one place: this is the published duplicate.
			name: "same name, kind and parent",
			in: []Location{
				{UID: 1181, Name: "Миколаївська територіальна громада", Kind: KindHromada, Parent: 114},
				{UID: 1180, Name: "Миколаївська територіальна громада", Kind: KindHromada, Parent: 114},
			},
			want: true,
		},
		{
			// Different raions. One region cannot be both, and mapping it to
			// either is a device announcing for somewhere else.
			name: "same name, different parent",
			in: []Location{
				{UID: 234, Name: "Вишнівська територіальна громада", Kind: KindHromada, Parent: 40},
				{UID: 296, Name: "Вишнівська територіальна громада", Kind: KindHromada, Parent: 42},
			},
			want: false,
		},
		{
			name: "same name and parent, different kind",
			in: []Location{
				{UID: 500, Name: "Приклад", Kind: KindHromada, Parent: 114},
				{UID: 501, Name: "Приклад", Kind: KindRaion, Parent: 114},
			},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := onlyIndistinguishable(tc.in)
			if ok != tc.want {
				t.Fatalf("onlyIndistinguishable = %v, want %v", ok, tc.want)
			}
			if !ok {
				return
			}
			// Lowest UID, only so a regenerated table is reviewable: the
			// premise is that the choice does not matter, and a table that
			// moved between runs would be a diff nobody could read.
			if got.UID != 1180 {
				t.Errorf("picked UID %d, want the lowest (1180) for a deterministic table", got.UID)
			}
		})
	}
}
