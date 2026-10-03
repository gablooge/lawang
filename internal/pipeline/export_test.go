package pipeline

import (
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/record"
)

// NormalizedFor builds a Normalized out of records that were sealed somewhere else, under a
// version order the caller names.
//
// It is here for the tests that need something no provider in this repository can produce: an
// entity whose external id is longer in bytes than a supersede chain can be keyed by, and a
// delivery whose provider declared no version order at all (which the registry refuses at
// start-up, so Normalize cannot build one). Nothing in the program builds a Normalized any way
// but through Normalize.
func NormalizedFor(providerKey string, order provider.VersionOrder, recs []record.Record) Normalized {
	return Normalized{provider: providerKey, versions: order, records: recs}
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

// CompareVersions is the version order of ADR 12 decision 1, exported for the table test that
// pins it. No caller in the program needs it: the ledger is the only thing that orders a version,
// and it does so with the entity's head and the provider's declared spelling in hand.
var CompareVersions = compareVersions

// The masker's pure half, reachable from the external test package and from nowhere else.
//
// It is not exported from the package itself, and that is the point: findSecrets(r.Text).secrets()
// is a one-line way to get every address, number and account in a record in the clear, outside the
// redaction map, with no tenant and no audit trail, which is the one thing this package exists to
// prevent. A later item reaching for it in a log line or a metric label would undo the masking
// without touching a file the masker owns. The tests need it because the finding half is worth
// testing with no database in sight, so it lives here, the way RecordsOf, ValidIBAN, ValidPhone
// and EntityKeys already do.
type (
	// Secret is one value the masker found.
	Secret = foundSecret
	// Kind is the class of a found value.
	Kind = secretKind
)

// The kinds, under the names the tests read them by.
const (
	KindEmail = kindEmail
	KindPhone = kindPhone
	KindIBAN  = kindIBAN
)

// Scan is one string and everything the masker found in it.
type Scan struct{ inner textScan }

// Find is findSecrets.
func Find(text string) Scan { return Scan{findSecrets(text)} }

// Secrets is every distinct value the scan found, in the order they first appear.
func (s Scan) Secrets() []Secret { return s.inner.secrets() }

// Apply is the replacement half: every value swapped for its token, refusing a result longer than
// the field allows.
func (s Scan) Apply(field string, tokens map[Secret]string, maxChars int) (string, error) {
	return s.inner.apply(field, tokens, maxChars)
}

// EntityKeys is the order the entity locks of one delivery are taken in. It is exported for the
// test that pins that order, because a deadlock is what a wrong order costs and a deadlock is not
// something a test can produce on demand.
var EntityKeys = entityKeys
