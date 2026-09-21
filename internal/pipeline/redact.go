package pipeline

import (
	"context"
	"fmt"

	"github.com/gablooge/lawang/internal/ids"
	"github.com/gablooge/lawang/internal/pipeline/pipelinedb"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// token is the placeholder a masked value is replaced by: the kind, so an operator reading a
// record can see what was taken out, and a ULID, which says nothing about the value.
//
// It is deliberately not derived from the value. A token that were a hash of the address would
// hand every sink an oracle: anyone with a guess at an address could compute its token and confirm
// that the address appears in this tenant's records, which is the exact fact the masking is there
// to withhold. The mapping lives in the redaction_map table instead, which never leaves this
// deployment.
//
// Its characters are the ones the record format allows everywhere: ASCII letters, digits, a colon
// and brackets. A ULID is 26 characters, so a token is 34 or 33 characters long.
func token(k Kind) string { return "[" + string(k) + ":" + ids.New() + "]" }

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
	scans := make([]struct{ title, text Scan }, len(recs))
	var wanted []Secret
	seen := map[Secret]struct{}{}
	for i, r := range recs {
		scans[i].title = Find(r.Title)
		scans[i].text = Find(r.Text)
		for _, s := range append(scans[i].title.Secrets(), scans[i].text.Secrets()...) {
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			wanted = append(wanted, s)
		}
	}
	tokens, err := mapSecrets(ctx, q, tenant, wanted)
	if err != nil {
		return err
	}
	for i := range recs {
		title, err := scans[i].title.Apply("title", tokens, record.MaxTitle)
		if err != nil {
			return err
		}
		text, err := scans[i].text.Apply("text", tokens, record.MaxText)
		if err != nil {
			return err
		}
		recs[i].Title, recs[i].Text = title, text
	}
	return nil
}

// mapSecrets gives every secret the token that stands for it: the one the redaction map already
// holds, or a new one, stored now.
func mapSecrets(ctx context.Context, q *pipelinedb.Queries, tenant tenancy.ID, secrets []Secret) (map[Secret]string, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	tokens := make([]string, len(secrets))
	kinds := make([]string, len(secrets))
	values := make([]string, len(secrets))
	for i, s := range secrets {
		tokens[i] = token(s.Kind)
		kinds[i] = string(s.Kind)
		values[i] = s.Value
	}
	rows, err := q.UpsertRedactions(ctx, pipelinedb.UpsertRedactionsParams{
		TenantID: tenant.String(), Tokens: tokens, Kinds: kinds, Values: values,
	})
	if err != nil {
		// The error of a failed insert can quote the value it tried to store, which is the very
		// thing being masked, so it is not passed on.
		return nil, fmt.Errorf("pipeline: store %d redaction mappings", len(secrets))
	}
	out := make(map[Secret]string, len(rows))
	for _, row := range rows {
		out[Secret{Kind: Kind(row.Kind), Value: row.Value}] = row.Token
	}
	// Apply refuses a value with no token, so a short result fails there and nothing goes out
	// unmasked. Saying it here names the cause instead of the symptom.
	if len(out) != len(secrets) {
		return nil, fmt.Errorf("pipeline: the redaction map returned %d tokens for %d values", len(out), len(secrets))
	}
	return out, nil
}
