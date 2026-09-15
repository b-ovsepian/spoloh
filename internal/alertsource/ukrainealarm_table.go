// PLACEHOLDER — ukrainealarm_gen.go has not been run yet. DO NOT EDIT BY HAND.
//
// That generator writes this file from GET /api/v3/regions, which needs the
// operator's own API key and is therefore run by hand:
//
//	go run ukrainealarm_gen.go -key <the key mailed to you after registering>
//
// Until somebody runs it the map below is empty, every fetch from the
// ukrainealarm source fails with ErrNoRegionMapping, the cascade moves on to
// the next source, and the dashboard says the mapping is missing. That is why
// this ships empty rather than carrying ids typed in by hand: a wrong region
// id is a device announcing for somewhere else, which is the one failure this
// project cannot have, and a table nobody can check against the live API is
// exactly where a wrong one would hide.

package alertsource

// ukraineAlarmRegions maps a published alerts.in.ua location UID onto
// ukrainealarm.com's own id for the same place. A location with no entry is
// one this source cannot serve; see UkraineAlarmRegionID.
var ukraineAlarmRegions = map[int]string{}
