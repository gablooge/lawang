package pipeline

import "github.com/gablooge/lawang/internal/record"

// NormalizedFor builds a Normalized out of records that were sealed somewhere else.
//
// It is here for the one test that needs a record no provider in this repository can produce: an
// entity whose external id is longer in bytes than a supersede chain can be keyed by. Nothing in
// the program builds a Normalized any way but through Normalize.
func NormalizedFor(providerKey string, recs []record.Record) Normalized {
	return Normalized{provider: providerKey, records: recs}
}

// RecordsOf is what Normalize produced, for the tests that look at the records before they are
// prepared. It is here and not on Normalized because no caller in the program needs it: the worker
// hands what Normalize returned straight to Prepare.
func RecordsOf(n Normalized) []record.Record { return n.records }

// The two rules the masker's patterns cannot state, exported so that they can be tested on inputs
// the patterns themselves would never produce. Both are guards, and a guard whose only proof is
// that nothing reaches it is not proved at all.
var (
	ValidIBAN  = validIBAN
	ValidPhone = validPhone
)

// EntityKeys is the order the entity locks of one delivery are taken in. It is exported for the
// test that pins that order, because a deadlock is what a wrong order costs and a deadlock is not
// something a test can produce on demand.
var EntityKeys = entityKeys
