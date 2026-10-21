# engageledger

Keep the money terms of a contracting engagement as a signed append-only journal: price moves only by change orders signed by both parties, acceptance is a business-day clock stopped only by a rejection that names a criterion, and invoices are computed from the journal.

The journal is JSONL where each line carries the SHA-256 of the line before it, so a replay either reaches the end or names the first line it refuses; the chain is plumbing under the fold that prices the engagement.

Maintained by [polycratia](https://polycratia.com).
