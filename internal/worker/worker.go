// Package worker is the drain: the goroutine pool that claims outbox rows and carries each one
// to a sink, and the shutdown that leaves nothing half done. It is step 1 to step 7 of the drain
// path (docs/architecture.md, section 3.2).
//
// # The shape of one row's work
//
// A row is claimed across tenants and worked under its own tenant, and those are different
// transactions on purpose. The claim runs as lawang_worker, which is granted the few scheduling
// columns and nothing else; the work runs as the application role bound to the claimed row's
// tenant. Binding a tenant inside the worker role would narrow nothing, because Postgres ORs
// permissive policies together and the worker's own policy admits every tenant (architecture
// section 4, store.RoleTx). So:
//
//  1. Claim (cross-tenant, lawang_worker).
//  2. Read what the row needs, bound to its tenant.
//  3. Normalize, which talks to the provider's API, OUTSIDE every transaction.
//  4. Prepare: the ledger rows and the records this delivery was prepared into, committed
//     together with the row's move to prepared. One transaction, bound to the tenant.
//  5. Deliver to the sink, outside every transaction.
//  6. Finish: the row delivered, the records the sink refused dead, the rest delivered. One
//     transaction again.
//
// A crash between 4 and 6 loses nothing and repeats at most one delivery: the records are
// stored, so the next claim delivers exactly what the first commit decided, and a sink is
// idempotent on the record id. The one thing it must not do is prepare again, because the
// ledger holds what was prepared and would skip every record of its own delivery.
//
// # What a failure does
//
// Three outcomes, and the difference between them is what destroys a record and what does not.
//
//   - Retry (Fail): the backoff ladder, which dead-letters the row at its end. For everything
//     that may work on the next attempt: a provider's API, the database, a sink's 5xx.
//   - Dead letter (MarkDead): for what the same bytes will refuse again, which is
//     pipeline.ErrDeadLetter and nothing else.
//   - Halt: the row stays where it is, claimable again after a pause, and the ladder is not
//     walked. For everything that cannot come right by itself and must still never destroy a
//     record: the sink refusing the request rather than its contents, a tenant whose sink
//     cannot be built, and any answer a sink gives that its own contract does not allow. That
//     last one is the fail-closed default (principle: an unforeseen case stalls, it does not
//     destroy), and it is why a Deliver that returns something other than a *sink.Fault, or a
//     Fault with an action this package does not know, halts instead of retrying.
//
// A worker that dies leaves no outcome at all, and that case is the reason the drain looks at
// the attempt count before it does any work: see Drain.workRow.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/pipeline"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/sink"
	"github.com/gablooge/lawang/internal/store"
	"github.com/gablooge/lawang/internal/tenancy"
)

// Sinks answers which sink a tenant's records go to.
//
// An error is not a reason to drop anything: the drain halts the row, so nothing is delivered,
// nothing is dead-lettered and the row is offered again after the halt pause. That covers both
// shapes this can take, a tenant nobody has configured a sink for (an operator acts) and a
// vault that cannot be reached right now (it comes back by itself), without either of them
// spending the retry ladder.
//
// # An implementation returns one sink per tenant and keeps it
//
// Sink is called once per delivery, and a sink owns a connection pool: sink.NewHTTP gives each
// one a cloned *http.Transport of its own, because sharing http.DefaultTransport capped the
// idle connections per host at two and cost 1,480 failed deliveries against 209 (issue #9,
// measured on pull request #55). An implementation that builds a sink per call throws that
// measurement away and pays a TCP and TLS handshake per delivery instead, which is what the
// shape of this signature invites: a context and a tenant in, a sink out, which is what a vault
// lookup looks like.
//
// It also leaks. Neither sink.Sink nor sink.HTTP has a Close, so nothing calls
// CloseIdleConnections on a transport that is dropped, and each abandoned one holds up to
// MaxIdleConnsPerHost sockets open until IdleConnTimeout (90 seconds by default). At one
// delivery a second that is up to 90 live transports at once.
//
// So: cache by tenant, build on a miss, and when a tenant's configuration changes, call
// CloseIdleConnections on the transport of the sink being replaced before dropping it. A sink
// is safe for concurrent use, and the drain calls one from every goroutine of the pool.
type Sinks interface {
	Sink(ctx context.Context, t tenancy.ID) (sink.Sink, error)
}

// Defaults for Options.
const (
	// DefaultBatch is how many rows one claim leases. The claim costs what it returns (ADR 10),
	// so this trades round trips for the time a goroutine holds leases it has not reached yet.
	DefaultBatch = 5
	// DefaultLease is how long a claim holds a row. It is also how long a crashed worker's rows
	// wait before another replica may take them, and nothing of those entities moves meanwhile,
	// so it is minutes and not hours.
	DefaultLease = 2 * time.Minute
	// DefaultRowTimeout bounds one row's whole journey, hydration and sink call included. It is
	// under DefaultLease so that an ordinary slow row finishes inside its own lease.
	DefaultRowTimeout = 90 * time.Second
	// DefaultPoll is how long a goroutine waits after a claim that returned nothing. A claim
	// that returns rows is followed at once by the next one.
	DefaultPoll = time.Second
	// DefaultHaltPause is how long a halted row waits before it is claimed again. It is the
	// interval at which a deployment finds out that an operator has fixed the credential, the
	// endpoint or the receiver, so it is short enough to be unnoticeable and long enough that a
	// halting sink is not a poll loop.
	DefaultHaltPause = time.Minute
	// DefaultPool is the most drain goroutines Options defaults to. The real default is bounded
	// by the connection pool as well: see NewDrain.
	DefaultPool = 4
)

// Options configure a Drain. Every field has a default, and NewDrain refuses the combinations
// that cannot work.
type Options struct {
	// Pool is how many rows are worked at once. Zero means the smaller of DefaultPool and what
	// the database pool can carry (NewDrain).
	Pool int
	// Batch is how many rows one claim leases. Zero means DefaultBatch.
	Batch int
	// Lease is how long a claim holds its rows. Zero means DefaultLease.
	Lease time.Duration
	// RowTimeout bounds one row's work. Zero means DefaultRowTimeout. It must be under Lease.
	RowTimeout time.Duration
	// Poll is the wait after an empty claim. Zero means DefaultPoll.
	Poll time.Duration
	// HaltPause is how long a halted row waits. Zero means DefaultHaltPause.
	HaltPause time.Duration
	// Ladder is the retry schedule. The zero value means outbox.DefaultLadder. It is also what
	// decides when a row nobody reported on is abandoned (outbox.Ladder.Exhausted).
	Ladder outbox.Ladder
	// Logger is where the drain writes. Zero means slog.Default.
	Logger *slog.Logger
}

// Drain claims outbox rows and delivers them. One Drain runs Options.Pool goroutines.
type Drain struct {
	db    *store.DB
	ob    *outbox.Outbox
	pl    *pipeline.Pipeline
	sinks Sinks
	opts  Options
	log   *slog.Logger
}

// NewDrain builds a Drain, or reports what is wrong with the configuration.
//
// The pool is sized against the database pool, which every goroutine takes a connection from
// for as long as a row's transaction lasts, and which the sweeps of a worker process share. A
// drain with more goroutines than the database pool has connections does not drain faster: it
// waits on the pool, and starves everything else in the process that needs a connection. So an
// unset Pool is at most MaxConns-1, and a Pool that is asked for explicitly is refused if it
// leaves the pool nothing. The size of the database pool is pool_max_conns in
// LAWANG_DATABASE_URL, and pgx's default is max(4, NumCPU).
func NewDrain(db *store.DB, pl *pipeline.Pipeline, sinks Sinks, opts Options) (*Drain, error) {
	if db == nil || pl == nil || sinks == nil {
		return nil, errors.New("worker: a drain needs a database, a pipeline and a sink for each tenant")
	}
	conns := db.MaxConns()
	if opts.Pool == 0 {
		opts.Pool = min(DefaultPool, max(1, conns-1))
	}
	switch {
	case opts.Pool < 0:
		return nil, errors.New("worker: the drain pool cannot be negative")
	case opts.Pool >= conns:
		return nil, fmt.Errorf("worker: a drain pool of %d needs more than %d database connections, raise pool_max_conns in LAWANG_DATABASE_URL",
			opts.Pool, conns)
	}
	setDefault(&opts.Batch, DefaultBatch)
	setDefault(&opts.Lease, DefaultLease)
	setDefault(&opts.RowTimeout, DefaultRowTimeout)
	setDefault(&opts.Poll, DefaultPoll)
	setDefault(&opts.HaltPause, DefaultHaltPause)
	if opts.Ladder == nil {
		opts.Ladder = outbox.DefaultLadder
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	// A row that cannot finish inside its own lease is taken over by another worker while it is
	// still working, so every delivery is made twice and every transition it then tries is
	// refused. The sink's idempotence makes that safe and not free, and it would be the steady
	// state rather than an accident.
	if opts.RowTimeout >= opts.Lease {
		return nil, fmt.Errorf("worker: a row may take %s, which is not less than the %s lease on it",
			opts.RowTimeout, opts.Lease)
	}
	return &Drain{db: db, ob: outbox.New(db), pl: pl, sinks: sinks, opts: opts, log: opts.Logger}, nil
}

// setDefault fills in a zero value. Anything negative is left as it is and refused downstream by
// the outbox (a claim with a lease that is not positive, for example).
func setDefault[T ~int | ~int64](v *T, fallback T) {
	if *v == 0 {
		*v = fallback
	}
}

// Run drains until ctx is done, then returns once every row already claimed has finished.
//
// Shutdown is architecture section 9: cancel the context, stop claiming, let what is in flight
// finish. A row that is already being worked runs on to its last transition under a context of
// its own (context.WithoutCancel plus RowTimeout), because stopping halfway is what leaves a
// lease on a row nobody is working and an entity behind it stalled. Rows that were claimed and
// not reached are released, so another replica takes them at once and the attempt they never
// had is not charged to them.
func (d *Drain) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := range d.opts.Pool {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.loop(ctx, i)
		}()
	}
	wg.Wait()
	return nil
}

// loop is one drain goroutine.
func (d *Drain) loop(ctx context.Context, n int) {
	log := d.log.With("drain", n)
	for ctx.Err() == nil {
		worked, err := d.once(ctx)
		switch {
		case err != nil:
			// A claim that fails is the database, not a row. Nothing is leased and nothing is
			// lost; wait out the poll rather than spin. A claim that was cut off by the
			// shutdown itself is not an incident and is not logged as one.
			if ctx.Err() == nil {
				log.Error("the outbox claim failed", "error", err)
			}
			d.wait(ctx)
		case worked == 0:
			d.wait(ctx)
		}
	}
}

// wait sleeps for the poll interval, or returns at once when the drain is shutting down.
func (d *Drain) wait(ctx context.Context) {
	timer := time.NewTimer(d.opts.Poll)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// once claims a batch and works it, and reports how many rows it leased.
//
// The rows of one batch are worked one after the other, so one goroutine is one row at a time
// and the batch is only a way of paying for one claim instead of several. A batch that takes
// longer than the lease is possible and is safe: every transition is guarded by the lease
// token, so a row taken over meanwhile is refused rather than written twice, and the sink's
// idempotence makes the repeated delivery a no-op.
//
// The bound an operator should know is Batch x RowTimeout against Lease, and the defaults do
// NOT keep a batch inside a lease: 5 rows at 90 seconds is 450 seconds against a 2 minute
// lease, and it does not take the worst case to get there, since the http sink's own default
// timeout is 30 seconds and three rows that each hit it already exceed the lease. NewDrain
// relates RowTimeout to Lease and deliberately not the product, because clamping the product
// would mean either a batch of one or a lease measured in tens of minutes, and a long lease is
// how long a crashed worker's rows sit undeliverable.
//
// So this is the real bound: while a sink is slow rather than broken, the tail of a batch is
// worked after its own lease has expired, those rows are delivered a second time by whoever
// claimed them next (safe, by idempotence, and paid for), every transition on them is refused,
// and the drain logs "could not record a delivery that was made" and "the lease was lost
// before the delivery was recorded" once per such row. An operator seeing those together is
// looking at a slow sink and should lower Batch or raise Lease.
func (d *Drain) once(ctx context.Context) (int, error) {
	claimed, err := d.ob.Claim(ctx, d.opts.Batch, d.opts.Lease)
	if err != nil {
		return 0, err
	}
	for i, c := range claimed {
		if ctx.Err() != nil {
			// Shutting down. What is left of the batch was never started, so it is given back
			// rather than left to its lease.
			d.release(claimed[i:])
			break
		}
		d.work(ctx, c)
	}
	return len(claimed), nil
}

// release gives back rows this worker claimed and will not reach.
func (d *Drain) release(claimed []outbox.Claimed) {
	// Not on the cancelled context: the whole point is to run after the shutdown began. The
	// bound is the row timeout, which is more than enough for one statement per row.
	ctx, cancel := context.WithTimeout(context.Background(), d.opts.RowTimeout)
	defer cancel()
	for _, c := range claimed {
		if err := d.ob.Release(ctx, c); err != nil {
			// Nothing is lost: the lease runs out and another worker takes the row over.
			d.log.Warn("could not give a claimed row back on shutdown", "outbox_id", c.ID(), "error", err)
		}
	}
}

// work carries one row from its claim to its outcome, under a context of its own.
func (d *Drain) work(parent context.Context, c outbox.Claimed) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), d.opts.RowTimeout)
	defer cancel()
	log := d.log.With("outbox_id", c.ID(), "tenant", c.Tenant().String(), "attempt", c.Attempt())
	defer func() {
		// A panic in one row must not take the pool down with it, and it must not be swallowed
		// either. The row keeps its lease, which runs out, and the next claims of it count up
		// to the abandonment below: a row that panics on every attempt is parked rather than
		// taken over forever. The panic value is logged and the stack is not, because a stack
		// is where a secret in an argument would be printed.
		if r := recover(); r != nil {
			log.Error("the drain panicked while working a row, and the row keeps its lease until it expires",
				"panic", fmt.Sprint(r))
		}
	}()
	d.workRow(ctx, log, c)
}

// workRow is one row, with the panic guard and the context already around it.
func (d *Drain) workRow(ctx context.Context, log *slog.Logger, c outbox.Claimed) {
	// Before any work at all. attempts is incremented by the claim and read by nothing but
	// Fail, so a row whose worker dies or hangs on every attempt never records a failure: its
	// lease expires, it is claimed again, and it holds every later version of its entity behind
	// it forever. This is the only place that can see that, and it sees it before spending
	// another attempt on it.
	if d.opts.Ladder.Exhausted(c.Attempt()) {
		if err := d.ob.MarkAbandoned(ctx, c); err != nil {
			log.Error("could not park a row that no attempt has reported on", "error", err)
			return
		}
		log.Error("parked a row that has been claimed past the end of its ladder with no failure recorded")
		return
	}

	work, err := d.ob.Work(ctx, c)
	if err != nil {
		d.fail(ctx, log, c, outbox.NewCause(outbox.ClassInternal), err)
		return
	}
	recs, ok := d.readyRecords(ctx, log, c, work)
	if !ok {
		return
	}
	d.deliver(ctx, log, c, recs)
}

// readyRecords is the records this claim has to deliver: the ones its own Prepare just wrote, or
// the ones a previous attempt stored before it died. ok is false when the row has been dealt
// with (failed, dead-lettered or halted) and there is nothing to deliver.
func (d *Drain) readyRecords(ctx context.Context, log *slog.Logger, c outbox.Claimed, work outbox.Work) ([]record.Record, bool) {
	if work.State != outbox.StatePending {
		// Already prepared: its ledger rows are written, so deriving its records again would
		// skip every one of them. It is delivered from what the first commit stored.
		stored, err := d.ob.PreparedRecords(ctx, c)
		if err != nil {
			d.fail(ctx, log, c, outbox.NewCause(outbox.ClassInternal), err)
			return nil, false
		}
		recs := make([]record.Record, 0, len(stored))
		for _, s := range stored {
			// Everything the row holds about the record, not the document alone: the record id
			// the row is keyed on, and the op and kind the document's own seal cannot vouch
			// for (record.Stored).
			r, err := record.Reopen(record.Stored{
				ID: s.RecordID, Op: record.Op(s.Op), Kind: record.Kind(s.Kind), Document: s.Document,
			}, work.Provider, c.Tenant())
			if err != nil {
				// The document in the table is not this tenant's record, or not a record at
				// all. Another attempt reads the same bytes, so it is a dead letter, and the
				// records that are still fine go with it: this delivery cannot be made whole.
				d.dead(ctx, log, c, outbox.NewCause(outbox.ClassInternal).WithCode(codeStoredRecord), err)
				return nil, false
			}
			recs = append(recs, r)
		}
		return recs, true
	}

	// Outside every transaction: it talks to the provider's API (architecture principle 6, and
	// store.begin, which relabels a network error returned from inside a transaction).
	n, err := d.pl.Normalize(ctx, pipeline.Delivery{
		Tenant: c.Tenant(), Provider: work.Provider, ID: c.ID(), Body: work.Body,
	})
	if err != nil {
		// Normalize's one outside call is the provider's, so a failure that is not a dead
		// letter is the provider's API, and the error may carry the provider's own text
		// (outside says why that is not logged as it is).
		d.failPipeline(ctx, log, c, outbox.ClassProviderUnavailable, err, outside(err))
		return nil, false
	}

	var out pipeline.Prepared
	err = d.db.TenantTx(ctx, c.Tenant(), func(tx pgx.Tx) error {
		var err error
		out, err = d.pl.Prepare(ctx, tx, c.Tenant(), n)
		if err != nil {
			return err
		}
		docs, err := documentsOf(out.Records)
		if err != nil {
			return err
		}
		// The ledger rows, the records and the row's move to prepared, in one commit. That is
		// what makes a crash from here on a repeat rather than a loss.
		return d.ob.PrepareIn(ctx, tx, c, docs)
	})
	// On the error path too: the counters are what the stage decided before it stopped, and
	// ScopeReturned and VersionUnordered live only here until B25 reads them into a metric
	// (ADR 4 decision 7, ADR 12 decision 1).
	log.Info("prepared",
		"records", len(out.Records), "skipped", out.Skipped, "stale", out.Stale,
		"scope_returned", out.ScopeReturned, "version_unordered", out.VersionUnordered,
		"automation", out.Automation, "degraded", out.Degraded)
	if err != nil {
		if errors.Is(err, outbox.ErrLeaseLost) {
			log.Info("the lease was lost before this delivery was prepared, so another worker has it")
			return nil, false
		}
		// Prepare, documentsOf and PrepareIn are all this program's own code, talking to this
		// program's own database, so the error is logged as it is.
		//
		// What makes that safe is a property of pgx rather than of this code, and it is worth
		// writing down because nothing here would notice it changing: a statement that refuses
		// a row carries Postgres's DETAIL, which for a CHECK or a unique violation on
		// outbox_record is "Failing row contains (...)" with the stored document in it, and
		// pgconn.PgError.Error() prints the severity, the message and the SQLSTATE and not the
		// Detail. If that ever stops being true, this is the call site that starts quoting a
		// record's content into a log line, and it would need the same treatment as outside.
		d.failPipeline(ctx, log, c, outbox.ClassInternal, err, err)
		return nil, false
	}
	return out.Records, true
}

// documentsOf marshals each prepared record into the document a sink receives, which is also
// what is stored for the delivery that follows a crash.
//
// Marshalling validates the record and checks its seal, so a record this stage would refuse to
// send is refused before it is stored rather than after. internal/pipeline has already made
// both true; this is where it would show if it ever stopped being true.
func documentsOf(recs []record.Record) ([]outbox.PreparedRecord, error) {
	out := make([]outbox.PreparedRecord, 0, len(recs))
	for _, r := range recs {
		doc, err := r.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("%w: a prepared record cannot be written: %w", pipeline.ErrDeadLetter, err)
		}
		// Op and Kind go beside the document, because the id does not hash them and a decoded
		// document's seal is recomputed from its own values: see record.Stored.
		out = append(out, outbox.PreparedRecord{
			RecordID: r.ID, Document: doc, Op: string(r.Op), Kind: string(r.Kind),
		})
	}
	return out, nil
}

// deliver hands the records to the tenant's sink and records what came back.
func (d *Drain) deliver(ctx context.Context, log *slog.Logger, c outbox.Claimed, recs []record.Record) {
	if len(recs) == 0 {
		// Every record of this delivery was one the ledger already held. There is nothing to
		// offer, and the row is finished rather than left for the ladder.
		d.finish(ctx, log, c, nil)
		return
	}
	snk, err := d.sinks.Sink(ctx, c.Tenant())
	if err != nil {
		// Sinks is the operator's own code, and from B13 a vault client: outside says why its
		// error is not logged as it is.
		d.halt(ctx, log, c, outbox.NewCause(outbox.ClassInternal).WithCode(codeNoSink), outside(err))
		return
	}

	// Outside every transaction, for the same reason as Normalize.
	result, err := snk.Deliver(ctx, c.Tenant(), recs)

	var fault *sink.Fault
	switch {
	case errors.As(err, &fault):
		// A Fault built in internal/sink carries nothing but constants of this program, and a
		// Fault from a sink this repository did not write may not: outside is where the two
		// are told apart.
		why := outside(err)
		switch fault.Action {
		case sink.ActionRetry:
			d.fail(ctx, log, c, fault.Cause, why)
		case sink.ActionHalt:
			d.halt(ctx, log, c, fault.Cause, why)
		default:
			// Every other Action, sink.ActionUnset (the zero value, which no Fault built in
			// internal/sink carries) and any action a later version of that package adds that
			// this one does not know. There is deliberately no arm of its own for the zero
			// value: it would be byte-identical to this one, so the compiler could not tell
			// the two apart and neither could a reader asking which arm a given Fault took.
			// An unforeseen case stalls, because a retry here ends at a dead letter.
			d.halt(ctx, log, c, outbox.NewCause(outbox.ClassInternal).WithCode(codeSinkContract), why)
		}
	case err != nil:
		// A sink reports a failed delivery as a *sink.Fault and in no other way. Something else
		// is a sink that is broken, and nothing is known about what reached the receiver, so
		// the row stalls instead of walking a ladder that ends in a dead letter. It is also a
		// value from outside: a sink that does not keep the error contract is the last one to
		// trust with the text of its error.
		d.halt(ctx, log, c, outbox.NewCause(outbox.ClassInternal).WithCode(codeSinkContract), outside(err))
	default:
		dead, err := deadRecords(result.Rejected, recs)
		if err != nil {
			// The answer covers records that were not offered, or one twice, so it says
			// nothing trustworthy about the ones that were. Nothing is marked delivered and
			// the batch goes again (architecture section 11).
			d.fail(ctx, log, c, outbox.NewCause(outbox.ClassSinkUnreadable), err)
			return
		}
		for _, r := range result.Rejected {
			// Detail is not stored, so the log line is the only place it is ever seen. It is
			// one of internal/sink's own phrases when internal/sink built the Rejection, and
			// anything at all when another sink did, so it is printed only while that package
			// vouches for it. The record id needs no such check: deadRecords has already
			// refused an answer naming anything that was not in the batch, so by here every id
			// is one of ours.
			detail := r.Detail
			if !sink.KnownDetail(detail) {
				detail = "withheld: not a phrase internal/sink knows"
			}
			log.Warn("the sink refused a record", "record_id", r.ID, "cause", r.Cause.String(), "detail", detail)
		}
		d.finish(ctx, log, c, dead)
	}
}

// maxWhyDepth bounds how far outside unwraps an error chain. A chain is built by whoever
// returned the error, so its length is theirs to choose, and a log attribute is not the place
// to find out how long it is.
const maxWhyDepth = 8

// outsideText is what the drain prints about an error it did not build. It is an error so that
// it goes into the same "why" attribute as the ones the drain did build.
type outsideText string

func (t outsideText) Error() string { return string(t) }

// outside reduces an error that reached the drain across one of the three interfaces somebody
// else implements, provider.Provider (through pipeline.Normalize), Sinks and sink.Sink, to
// something this package is willing to write down.
//
// The text of such an error is not ours to trust. net/http returns a *url.Error whose text
// quotes the request URL with its query string, which is where several receivers and several
// provider APIs take their API key, and issue #9's acceptance is that this exact text appears
// in nothing a failed delivery produces. internal/sink removed it from everything it hands
// back; logging err.Error() here would put it back one layer up, in a log line, which is where
// the project already decided it must not be.
//
// A *sink.Fault is this program's own type, but a Sink this repository did not write builds one
// too, and then only its shape is ours. Of its three fields the Detail is the one this function
// can decide: it is a phrase from a list internal/sink owns, so sink.KnownDetail is asked rather
// than assumed, and a phrase that passes is what comes back, because it is the one readable
// thing a broken sink gives an operator.
//
// The Cause is a different thing and this function does not touch it. A Cause carries a class of
// this program's own and a code, and the code is chosen by the receiver or by the sink:
// sink.Rejection.Detail says so in as many words, and outbox.Cause.WithCode is only a shape
// filter (at most 64 bytes of [A-Za-z0-9_.-], which its own doc comment says cannot stop a
// caller that passes a secret as the code). That code is kept on purpose, because a receiver's
// own error code is what makes a dead letter actionable, and it is stored: every caller of this
// function passes the same Cause to outbox.Fail, outbox.Halt or outbox.MarkDead, which write
// Cause.String() into outbox.last_error, and sink.Rejection.Cause takes the same route into
// outbox_record.last_error through deadRecords. So the drain does write down one thing a sink
// chose. It is the Cause, deliberately, under B04's decision, and
// TestNoThirdPartyTextReachesAnythingTheDrainWrites pins which log attribute and which column
// hold it.
//
// What this function must therefore not do is hand back a value that reprints the Cause a
// second time somewhere nothing says it is. *sink.Fault is such a value: Fault.Error() prints
// Cause.String(), and KnownDetail("") is true, so returning the Fault itself put the sink's code
// into the "why" attribute of a line whose "cause" attribute already carries it, and put it
// there for every Fault, including the ones with no Detail at all. The Detail alone comes back
// instead, and a Fault with no Detail falls through to the chain below like anything else.
//
// Everything else becomes the chain of concrete types, which names the shape of the failure
// without quoting any value the failing call was given. The cost is real: an operator debugging
// a provider outage gets "*url.Error wrapping *net.OpError" and the Cause, and not the message
// the provider wrote. That is the same trade internal/sink made one layer down, and a provider
// that wants its message read should return an error of its own with nothing of the request in
// it (noted on issue #11).
func outside(err error) error {
	var fault *sink.Fault
	if errors.As(err, &fault) && fault.Detail != "" && sink.KnownDetail(fault.Detail) {
		return outsideText(fault.Detail)
	}
	var b strings.Builder
	for i := 0; err != nil && i < maxWhyDepth; i++ {
		if i > 0 {
			b.WriteString(" wrapping ")
		}
		fmt.Fprintf(&b, "%T", err)
		err = errors.Unwrap(err)
	}
	if err != nil {
		b.WriteString(" wrapping more")
	}
	return outsideText(b.String())
}

// deadRecords turns the sink's rejections into the outbox's per-record dead letters, and refuses
// an answer that does not line up with what was offered.
//
// A DeliveryResult means "the records named here were refused and every other record of the
// batch was taken", so an id that was not in the batch, or one named twice, makes the whole
// sentence unreadable: the outbox would be told to kill a record of some other delivery, or to
// mark delivered a record the sink refused under its other mention.
//
// Neither error quotes the id. An id that was not in the batch is a string the sink chose, and
// this error is logged, so quoting it would be the drain writing down a value a sink invented
// (issue #9). The id that was offered is in the delivery's own records, where an operator
// looking at the row can see it; what the sink made up belongs in what the sink logs.
func deadRecords(rejected []sink.Rejection, offered []record.Record) ([]outbox.DeadRecord, error) {
	if len(rejected) == 0 {
		return nil, nil
	}
	inBatch := make(map[string]bool, len(offered))
	for _, r := range offered {
		inBatch[r.ID] = true
	}
	dead := make([]outbox.DeadRecord, 0, len(rejected))
	seen := make(map[string]bool, len(rejected))
	for _, r := range rejected {
		switch {
		case !inBatch[r.ID]:
			return nil, errors.New("worker: the sink refused a record that was not in the batch")
		case seen[r.ID]:
			return nil, errors.New("worker: the sink refused one record twice")
		}
		seen[r.ID] = true
		dead = append(dead, outbox.DeadRecord{RecordID: r.ID, Cause: r.Cause})
	}
	return dead, nil
}

// The codes this package puts in a Cause for a failure of Lawang's own. Each is a constant of
// this program, which is what outbox.Cause.WithCode asks of a code, and without one the dead
// letter or the last error would read as a bare "internal error".
const (
	// codeNoSink: this tenant's sink could not be built at all.
	codeNoSink = "no_sink"
	// codeSinkContract: the sink answered in a way the Sink interface does not allow.
	codeSinkContract = "sink_contract"
	// codeStoredRecord: a stored record document is not this tenant's record any more.
	codeStoredRecord = "stored_record"
)

// finish marks the row delivered, with the records the sink refused as per-record dead letters.
func (d *Drain) finish(ctx context.Context, log *slog.Logger, c outbox.Claimed, dead []outbox.DeadRecord) {
	switch err := d.ob.MarkDelivered(ctx, c, dead); {
	case errors.Is(err, outbox.ErrLeaseLost):
		log.Info("the lease was lost before the delivery was recorded, so another worker has the row")
	case err != nil:
		// The delivery was made and could not be recorded. The lease runs out, the row is
		// claimed again, and the sink's idempotence makes the repeat a no-op.
		log.Error("could not record a delivery that was made", "error", err)
	default:
		log.Info("delivered", "dead_letters", len(dead))
	}
}

// fail puts the row back on the ladder, which dead-letters it at the end of it.
func (d *Drain) fail(ctx context.Context, log *slog.Logger, c outbox.Claimed, cause outbox.Cause, why error) {
	switch err := d.ob.Fail(ctx, c, d.opts.Ladder, cause); {
	case errors.Is(err, outbox.ErrLeaseLost):
		log.Info("the lease was lost before the failure was recorded", "cause", cause.String())
	case err != nil:
		log.Error("could not record a failure", "cause", cause.String(), "error", err, "why", why)
	default:
		log.Warn("the delivery failed and will be tried again", "cause", cause.String(), "why", why)
	}
}

// failPipeline is fail or dead, by the one distinction internal/pipeline makes: an error that
// wraps ErrDeadLetter is one the same bytes produce again.
//
// err decides, and why is what is written down. The two differ on the one path where the error
// may carry a provider's own text: see outside.
func (d *Drain) failPipeline(ctx context.Context, log *slog.Logger, c outbox.Claimed, class outbox.Class, err, why error) {
	if errors.Is(err, pipeline.ErrDeadLetter) {
		d.dead(ctx, log, c, outbox.NewCause(outbox.ClassNormalizer), why)
		return
	}
	d.fail(ctx, log, c, outbox.NewCause(class), why)
}

// dead parks the row, for a failure that the same bytes will produce again.
func (d *Drain) dead(ctx context.Context, log *slog.Logger, c outbox.Claimed, cause outbox.Cause, why error) {
	switch err := d.ob.MarkDead(ctx, c, cause); {
	case errors.Is(err, outbox.ErrLeaseLost):
		log.Info("the lease was lost before the dead letter was recorded", "cause", cause.String())
	case err != nil:
		log.Error("could not record a dead letter", "cause", cause.String(), "error", err, "why", why)
	default:
		log.Error("the delivery was parked as a dead letter", "cause", cause.String(), "why", why)
	}
}

// halt leaves the row where it is, to be claimed again after the pause, with no attempt charged
// and no record killed.
func (d *Drain) halt(ctx context.Context, log *slog.Logger, c outbox.Claimed, cause outbox.Cause, why error) {
	switch err := d.ob.Halt(ctx, c, d.opts.HaltPause, cause); {
	case errors.Is(err, outbox.ErrLeaseLost):
		log.Info("the lease was lost before the halt was recorded", "cause", cause.String())
	case err != nil:
		log.Error("could not record a halt", "cause", cause.String(), "error", err, "why", why)
	default:
		// At error level: nothing here recovers on its own, and an operator has to change
		// something (architecture section 11 asks for ops to be alerted).
		log.Error("the delivery is halted until something is changed, and nothing was delivered or killed",
			"cause", cause.String(), "why", why, "pause", d.opts.HaltPause.String())
	}
}
