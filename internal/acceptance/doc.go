// Package acceptance holds the M1 acceptance suite: the whole pipeline, end to end, in one
// process, proven on ClickUp (docs/roadmap.md, milestone M1, and backlog item B12).
//
// It has no code of its own. Everything is in the test files, because the suite composes packages
// that are already built rather than adding anything to the program, and nothing the program runs
// may depend on a test harness.
//
// # The two halves
//
// The fake-server half ([m1_test.go]) runs in CI on every pull request. It wires the real edge,
// the real hub, the real ClickUp provider, the real pipeline, the real drain and the strict stub
// sink over a real Postgres and a real socket, and points the provider's API client at a fake
// ClickUp server that answers out of internal/provider/clickup/testdata. Each of the five lines of
// M1's "Done when" has one test that demonstrates it, and each of those tests also says which
// layer produced the refusal, because a test that only watches a status code cannot tell the layer
// it names from any other layer that would answer the same way.
//
// The live half ([live_clickup_test.go]) is behind the `live` build tag and the LAWANG_LIVE_CLICKUP
// variable, and it is never in `make check` and never in CI. It is the only thing that can prove
// Verify agrees with ClickUp: a committed signature fixture can never be a recorded one, because
// the real webhook secret may not be committed, so the signature in a fixture is always one the
// tests signed themselves (issue #12, and testdata/README.md). The live half takes a delivery
// ClickUp itself signed, through the tunnel, and watches it verify.
//
// # Running the live half
//
// It needs the maintainer's own credentials and a public tunnel, so it is theirs to run, once, by
// hand. In two terminals:
//
//	# 1. the tunnel, which forwards ^/ingress/[a-z][a-z0-9_]{0,31}$ to port 8080
//	make tunnel
//
//	# 2. the run itself
//	export LAWANG_LIVE_CLICKUP=1
//	export LAWANG_LIVE_CLICKUP_TEAM=<the test workspace (team) id>
//	export LAWANG_LIVE_CLICKUP_TASK=<a task in the designated test list>
//	make acceptance-live
//
// The two ids are not secrets, but they are the maintainer's own, so they are passed in the
// environment of the one command and are never written into this repository. Everything secret
// stays in LAWANG_CREDENTIALS_DIR (default ~/.config/lawang): CLICKUP_TOKEN from clickup.env and
// the tunnel hostname from tunnel.env or the environment. A file or a variable that is absent is a
// skip with a message that names it, never a default and never a value.
//
// What one run does to the real account, and nothing else:
//
//   - creates one ClickUp webhook on the test workspace, pointing at https://<hostname>/ingress/clickup;
//   - posts one comment on LAWANG_LIVE_CLICKUP_TASK, whose text says it is an automated check;
//   - deletes both again when the test ends, and says loudly what it could not delete.
//
// Everything else it does is a read. Stop the tunnel container when the run is done, and check
// ClickUp's own webhook list for anything pointing at the hostname that the run left behind: a
// webhook nobody deregistered goes on posting deliveries at a developer machine.
//
// # The fixtures, and why this suite reads them from another package's testdata
//
// The fake-server half loads its webhook bodies and API answers from
// internal/provider/clickup/testdata, the directory B11 synthesized from ClickUp's published
// documentation and that B12's live run is to replace with real recordings. Reading them from
// there rather than keeping a second copy here is what makes the replacement a drop-in: the day
// the recorded payloads land in that directory, this suite runs against the real shapes with no
// change here at all. The cost is a relative path out of this package, which is written down in
// one function (fixture) and nowhere else.
package acceptance
