package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gablooge/lawang/internal/ids"
	"github.com/gablooge/lawang/internal/pipeline/pipelinedb"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// ErrTooManySecrets reports a delivery carrying more distinct values than one of them may map.
//
// Everything the masker finds becomes a permanent row in redaction_map, in one statement, and the
// text it scans comes from a sender: a webhook body is bounded only by ingress.DefaultMaxBody and
// a record's text only by record.MaxText, both 1 MiB, which is tens of thousands of distinct
// addresses if somebody wants it to be. Without a bound here, one delivery decides how large a
// single INSERT is and how many rows of personal data this deployment keeps forever.
//
// So there is a named bound, the way MaxExternalIDBytes is a named bound, and crossing it is a
// refusal by name rather than an unbounded statement.
var ErrTooManySecrets = errors.New("pipeline: the delivery holds more distinct values than the redaction map takes at once")

// MaxSecretsPerDelivery is the most distinct (kind, value) pairs one delivery may map, counted
// across every record in it.
//
// 1,024 is far above content and far below abuse. A delivery is one webhook body: a thread of
// messages, a task and its comments, a batch of changes. Real content in one of those holds a
// handful of addresses and occasionally a few dozen; the reviewer's 500 KB probe of nothing but
// addresses produced 20,445, and a 1 MiB one produces about 42,000. A delivery above this bound is
// not content, and the fix is on the normalizer, which decides how much of a provider's payload
// becomes one record.
const MaxSecretsPerDelivery = 1024

// token is the placeholder a masked value is replaced by: the kind, so an operator reading a
// record can see what was taken out, and a random ULID, which says nothing about the value.
//
// It is deliberately not derived from the value. A token that were a hash of the address would
// hand every sink an oracle: anyone with a guess at an address could compute its token and confirm
// that the address appears in this tenant's records, which is the exact fact the masking is there
// to withhold. The mapping lives in the redaction_map table instead, which never leaves this
// deployment.
//
// The entropy comes from ids.NewUnpredictable and not from ids.New, whose own doc comment forbids
// this use: New draws its 80 non-clock bits from math/rand seeded once at process start, so one
// observed id narrows the seed to a searchable set and two ids minted in the same millisecond
// differ by a small increment. Neither is a disclosure of a value on its own, and both are
// avoidable. What they would cost is that a placeholder planted in source text could be made to
// collide with a real mapping an operator later resolves, and that the gap between two tokens
// would say how many values this deployment masked in between, across tenants.
//
// Its characters are the ones the record format allows everywhere: ASCII letters, digits, a colon
// and brackets. A ULID is 26 characters, so a token is 34 or 33 characters long.
func token(k secretKind) (string, error) {
	id, err := ids.NewUnpredictable()
	if err != nil {
		return "", fmt.Errorf("pipeline: mint a %s placeholder: %w", k, err)
	}
	return "[" + string(k) + ":" + id + "]", nil
}

// maskRecords replaces every email address, telephone number and IBAN in the title and the text of
// each record with a token, and records what each token stands for in the redaction map.
//
// It is one statement for the whole delivery however many values were found, because the scan runs
// over every record first and the upsert takes arrays.
//
// The records are changed in place. Title and Text are not part of the seal (ADR 4, decision 7),
// so a masked record still marshals; the id, the external id, the version and the scope are
// untouched, which is what keeps the masker from changing anybody's identity.
func maskRecords(ctx context.Context, q *pipelinedb.Queries, tenant tenancy.ID, recs []record.Record) error {
	scans := make([]struct{ title, text textScan }, len(recs))
	var wanted []foundSecret
	seen := map[foundSecret]struct{}{}
	for i, r := range recs {
		scans[i].title = findSecrets(r.Title)
		scans[i].text = findSecrets(r.Text)
		for _, s := range append(scans[i].title.secrets(), scans[i].text.secrets()...) {
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			wanted = append(wanted, s)
		}
	}
	if len(wanted) > MaxSecretsPerDelivery {
		return fmt.Errorf("%w: %d distinct values", ErrTooManySecrets, len(wanted))
	}
	tokens, err := mapSecrets(ctx, q, tenant, wanted)
	if err != nil {
		return err
	}
	for i := range recs {
		title, err := scans[i].title.apply("title", tokens, record.MaxTitle)
		if err != nil {
			return err
		}
		text, err := scans[i].text.apply("text", tokens, record.MaxText)
		if err != nil {
			return err
		}
		recs[i].Title, recs[i].Text = title, text
	}
	return nil
}

// mapSecrets gives every secret the token that stands for it: the one the redaction map already
// holds, or a new one, stored now.
func mapSecrets(ctx context.Context, q *pipelinedb.Queries, tenant tenancy.ID, secrets []foundSecret) (map[foundSecret]string, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	tokens := make([]string, len(secrets))
	kinds := make([]string, len(secrets))
	values := make([]string, len(secrets))
	for i, s := range secrets {
		tok, err := token(s.Kind)
		if err != nil {
			// The entropy source failed, which is this deployment and not this delivery, so it
			// is not a dead letter and the next attempt may well work.
			return nil, err
		}
		tokens[i] = tok
		kinds[i] = string(s.Kind)
		values[i] = s.Value
	}
	rows, err := q.UpsertRedactions(ctx, pipelinedb.UpsertRedactionsParams{
		TenantID: tenant.String(), Tokens: tokens, Kinds: kinds, Values: values,
	})
	if err != nil {
		// The server's message and its detail field can quote the value the statement tried to
		// store, which is the very thing being masked, so neither is passed on. The SQLSTATE and
		// the constraint name can hold no value at all: one is a five character code and the other
		// is a name in the schema. They are what an operator needs, because a cancelled context,
		// a lost connection, a refusal by row-level security (42501), a value over the column's
		// CHECK (23514) and an arbiter conflict are one sentence without them, and only some of
		// them are worth retrying.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return nil, fmt.Errorf("pipeline: store %d redaction mappings: SQLSTATE %s, constraint %q",
				len(secrets), pgErr.Code, pgErr.ConstraintName)
		}
		// Not an answer from the server but a failure to get one: a cancelled context, a lost
		// connection, a pool that is closed. Nothing on that path has seen a value, and keeping the
		// error wrapped is what lets a caller ask errors.Is(err, context.Canceled).
		return nil, fmt.Errorf("pipeline: store %d redaction mappings: %w", len(secrets), err)
	}
	out := make(map[foundSecret]string, len(rows))
	for _, row := range rows {
		out[foundSecret{Kind: secretKind(row.Kind), Value: row.Value}] = row.Token
	}
	// Apply refuses a value with no token, so a short result fails there and nothing goes out
	// unmasked. Saying it here names the cause instead of the symptom.
	if len(out) != len(secrets) {
		return nil, fmt.Errorf("pipeline: the redaction map returned %d tokens for %d values", len(out), len(secrets))
	}
	return out, nil
}
