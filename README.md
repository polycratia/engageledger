# engageledger

Keep the money terms of a contracting engagement as a signed append-only journal: price moves only by change orders signed by both parties, acceptance is a business-day clock stopped only by a rejection that names a criterion, and invoices are computed from the journal.

The journal is JSONL where each line carries the SHA-256 of the line before it, so a replay either reaches the end or names the first line it refuses; the chain is plumbing under the fold that prices the engagement.

`terms.Fold` is that fold: it reads the events into the current milestones, the total price and the time-and-materials cap, all in minor units of the one contract currency. A number moves only when `change_order.approved` events from both the client and the contractor name the hash of the line the proposal was written on; proposed, declined and withdrawn change orders are listed next to the terms they left untouched. The fold names the first event it refuses, among them a change order naming an unknown milestone, one priced in another currency, one that removes or reprices an accepted milestone, and a second decision on a settled change order.

Maintained by [polycratia](https://polycratia.com).
