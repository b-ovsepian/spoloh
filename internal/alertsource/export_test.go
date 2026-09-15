package alertsource

import "testing"

// SetUkraineAlarmRegions replaces the generated id table for one test and
// restores it afterwards.
//
// It exists because the table this build ships is deliberately empty — no
// generator run has produced one yet (see ukrainealarm_table.go) — and a
// provider whose every test needed a real key and a live API would have no
// tests at all. The rows a test sets are stand-ins, and the parse they
// exercise is the same code a generated table will drive.
func SetUkraineAlarmRegions(t *testing.T, m map[int]string) {
	t.Helper()
	regions, uids := ukraineAlarmRegions, ukraineAlarmUIDs
	t.Cleanup(func() { ukraineAlarmRegions, ukraineAlarmUIDs = regions, uids })
	ukraineAlarmRegions, ukraineAlarmUIDs = m, reverseUkraineAlarmRegions(m)
}
