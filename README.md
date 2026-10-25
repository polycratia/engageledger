# engageledger

Keep the money terms of a contracting engagement as a signed append-only journal: price moves only by change orders signed by both parties, acceptance is a business-day clock stopped only by a rejection that names a criterion, and invoices are computed from the journal.

The journal is JSONL where each line carries the SHA-256 of the line before it, so a replay either reaches the end or names the first line it refuses; the chain is plumbing under the fold that prices the engagement.

`terms.Fold` is that fold: it reads the events into the current milestones, the total price and the time-and-materials cap, all in minor units of the one contract currency. A number moves only when `change_order.approved` events from both the client and the contractor name the hash of the line the proposal was written on; proposed, declined and withdrawn change orders are listed next to the terms they left untouched. The fold names the first event it refuses, among them a change order naming an unknown milestone, one priced in another currency, one that removes or reprices an accepted milestone, and a second decision on a settled change order.

An approval carries its evidence: `approval.Approval{party, method, evidence_hash, signature}`. An `ed25519` approval signs the canonical JSON of the proposal body and verifies against the key `contract.opened` lists for the party, so the journal alone can check it; a `recorded` approval - an email or an e-sign export kept outside the journal - leaves behind only the SHA-256 of that evidence, which nothing in the journal can tie to the body it stands for. The fold refuses a bad signature, an approval by someone who is not a party, a party the contract head lists no key for, and an approval naming a body hash other than the one it approves. Every effective change order reports the strength of its evidence - `signed`, `recorded`, or `asserted` where an approval carried no evidence at all - and `Terms.WeakTotal` adds up the money that moved on anything weaker than a signature.

Maintained by [polycratia](https://polycratia.com).
