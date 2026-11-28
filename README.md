# engageledger

Keep the money terms of a contracting engagement as a signed append-only journal: price moves only by change orders signed by both parties, acceptance is a business-day clock stopped only by a rejection that names a criterion, and invoices are computed from the journal.

## The problem

A contracting engagement is priced twice. Once in the statement of work both sides signed, and again in the spreadsheet somebody keeps while the work runs. The second one is where the invoices actually come from, and it has no history. A milestone is repriced in an email thread, a deliverable is rejected on a call, a cap is raised informally in a status meeting, and two months later nobody can say which number was in force on which day or who agreed to it. The argument that follows is not about what was agreed; it is about whose archive is better.

The other half of the problem is the clock. Engagement terms usually say a deliverable counts as accepted unless it is rejected within some number of business days. Business days depend on a zone, a weekend, a holiday list and a closing time, and each of those is a place where a deadline can move after the fact. If the deadline is recomputed every time somebody opens the file, a zoneinfo update or a corrected holiday list can shorten a window that is already running, which is a change to the contract made by a software upgrade. engageledger keeps the price, the acceptance clock and the not-to-exceed cap in one append-only JSONL file where every line carries the SHA-256 of the line before it, and it fixes each deadline at the moment of delivery. A replay either reaches the end or names the first line it refuses.

## The journal

Every line is the canonical JSON of `{seq, prev, type, body}`, where `prev` is the hex SHA-256 of the previous line's bytes. `journal.Replay` refuses the first line whose `seq` gaps or repeats, whose `prev` does not match, that is not valid UTF-8, or that is not byte-identical to its own canonical re-encoding — which is what catches the duplicate keys and the floats `encoding/json` would otherwise accept in silence. Amounts are integers in minor units of the one contract currency; `canon.Marshal` refuses a float outright, naming the JSON path that carries it.

The chain is plumbing. The decisions are in the folds.

## What the journal decides

### Signed change orders

`terms.Fold` reads the events into the current milestones, the total price and the time-and-materials cap. A number moves only when `change_order.approved` events from both the client and the contractor name the hash of the line the proposal was written on; proposed, declined and withdrawn change orders are listed next to the terms they left untouched.

**Refuses:** a change order naming a milestone the contract does not carry, one priced in another currency, one that removes or reprices an already accepted milestone, a second decision on a settled change order, an approval by someone who is not a party, an approval for a party the contract head lists no key for, an `ed25519` signature that does not verify, and an approval naming a body hash other than the one it approves.

**Protects both sides:** the client, because the price cannot grow without a line the client's own key signed; the contractor, because an approval that was given cannot later be unremembered. An approval carries its evidence — `approval.Approval{party, method, evidence_hash, signature}` — and an `ed25519` approval signs the canonical JSON of the proposal body, so the journal alone can check it. A `recorded` approval, an email or an e-sign export kept outside the journal, leaves only the SHA-256 of that evidence, which nothing here can tie to the body it stands for. Every effective change order therefore reports its evidence as `signed`, `recorded` or `asserted`, and `Terms.WeakTotal` adds up the money that moved on anything weaker than a signature.

### Criterion-cited rejection

`acceptance.Fold` reads the same journal into the acceptance state of every milestone: `delivered` while its window runs, then `accepted`, `rejected`, or `deemed_accepted` once the window runs out.

**Refuses:** a `milestone.rejected` event that names no criterion of that milestone, or names one with an empty defect note. Such a rejection fails no criterion, so it is listed among `Refused`, and the milestone stays `delivered` with its clock running. Any event on an accepted or deemed-accepted milestone is refused as well.

**Protects the contractor** against an open-ended "we are not happy with it" that stops the clock indefinitely, and **protects the client's** right to reject by making a cited rejection fully effective: it closes the window, and the next `milestone.delivered` opens a retest window that either restarts on the full term or resumes the days the closed one had left, as the `retest_window` of the contract head asks.

### A business-day clock fixed at delivery

The clock is `bizday.Calendar{version, zone, weekend, holidays, close}`. `Calendar.Hash` is the canonical SHA-256 the contract head carries, so two heads that spell the same calendar differently hash the same, and a replay can tell which calendar a deadline was computed on. `Calendar.Deadline(deliveredAt, n)` is the close of the n-th business day after a delivery, returned as one UTC instant: a delivery past close counts from the next business day, weekends and listed holidays are skipped, and the wall-clock close maps to a different instant on either side of a daylight-saving transition. The zoneinfo database is embedded, so the same journal reads the same deadline on any host.

`acceptance.Deliver` writes the deadline once, as `deadline_utc` next to the `calendar_hash` it was computed on, and the fold takes the instant as recorded. `Acceptance.Verify` recomputes every recorded deadline and returns a `Drift{seq, milestone, recorded, recomputed, cause}` for each one that now comes out elsewhere — `tzdata` where the closing day holds and only the instant moves, `calendar` where the day itself moves — **without writing any of them back**. `calendar.revised` appends another calendar version with its own hash, which later deliveries count on, while windows opened before it keep the version they named and the deadline they recorded.

**Refuses:** a delivery naming a calendar the journal never appended, or one a revision superseded.

**Protects the client,** because neither a zoneinfo update nor a holiday correction can shorten a window that is already running, and **protects the contractor,** because the date a milestone is deemed accepted on is a recorded instant rather than a recomputation. `Deem` moves a milestone to `deemed_accepted` only once that recorded instant has passed.

### Mutual holds

A `hold.proposed` event names a pause of one milestone between two dates; a `hold.approved` event carries one party's approval of that proposal, evidenced exactly as a change order approval is. A hold pauses nothing until every party has approved it: `Acceptance.Holds` lists a half-approved hold next to the effective ones, and only the approval that leaves no party without one makes it effective. An effective hold then extends its window once, through a `deadline.extended` event carrying a new UTC instant computed with the calendar version the paused window recorded.

**Refuses:** a hold that overlaps an effective hold of the same milestone, whether the overlap arrives with the proposal or with the approval that would make it effective; one starting after the recorded deadline has passed; one on a milestone with no open window; a second approval by the same party; an approval of a hold no `hold.proposed` before it carries; a second extension of one hold; an extension on a hold the parties have not all approved; one counting on another calendar version; one naming a milestone other than the hold pauses; and one that would move the deadline back.

**Protects both sides** from a unilateral pause: the contractor cannot stop the acceptance clock by declaring a hold, and the client cannot deny a pause both sides approved, because only a recorded extension moves a clock and a recorded extension cannot be taken back.

### The not-to-exceed cap

`nte.Fold` reads the cap and the time logged against it. A `time.logged` event carries the `minutes` worked and the `date` they were worked on; the cap is the `nte_cap_minutes` of the contract head, raised only by a change order carrying a `set_nte_cap` op that every party approved, which counts the cap version up.

**Refuses:** an entry that would carry the logged minutes past the cap in force — in full, with an error naming the minutes that remain. Nothing shortens an entry to make it fit, and a head that names no cap leaves no minutes to log.

**Protects the client** from an overrun that is discovered at invoicing time, and **protects the contractor** from working hours nobody will pay for: `Notify` appends the notices due at 75% and 90% of the cap as `nte.notice` events keyed by the SHA-256 of the contract, the cap version and the threshold, so a replay or a second run appends no notice twice, while a raised cap re-arms both thresholds under its new version.

### Billable events

`billable.Read` folds the journal into the terms, the acceptance state and the cap at once, and `billable.Derive` emits what may be billed: one event per `accepted` or `deemed_accepted` milestone at the price the terms carry, one per calendar month of logged time in the zone the contract counts days in at the `rate_minor_per_hour` of the `billing` config, and one per `billable.manual` line that references an effective change order. Only the minutes the cap let into `nte.Fold` are billed.

**Refuses:** a month whose minutes and rate do not come out in whole minor units, rather than rounding it, because rounding is an invoicing decision; a milestone that is accepted but no longer carried by the terms, because it has no price to bill; a manual line referencing a change order that is not effective; and a `billing` config that sets a tax rate, a tax amount, an invoice number or a buyer tax id, because a billable event carries a kind, an amount in minor units and the contract currency, and nothing beyond.

**Protects both sides** by making the invoice a derivation rather than an assertion: every line can be traced to the journal event that authorises it. `billable/testdata` pins the events derived from a sample contract.

## Quick start

```sh
make demo
```

That writes `example/engagement.jsonl` and runs three commands over it.

`seed` writes a seven-line engagement: three milestones (100000, 200000 and 50000 minor units of EUR) opened on a business-day calendar in `Europe/Berlin` closing at 17:00, with a 10 business-day acceptance window and a 300000 time-and-materials cap; one change order both parties signed with keys derived from their names; the first milestone delivered and accepted; and the second delivered with its window still running.

- `engageledger verify example/engagement.jsonl` walks the chain, the canonical form of every line, the approval evidence and the recorded deadlines, and exits 1 naming the first line it refuses, whichever fold refused it. A journal broken in several places still names the lowest line a reader has to look at.
- `engageledger fold example/engagement.jsonl` prints the milestones, the total, the cap and the evidence strength of every effective change order.
- `engageledger clock example/engagement.jsonl` prints the acceptance windows whose clock is running, each deadline both in UTC and in the zone the calendar its delivery named counts business days in.

Every command writes canonical JSON over integers and recorded instants alone, so one journal gives byte-identical output on every run and every machine. `make test` runs the suite, including a fuzz target over `journal.Replay`.

As a library:

```go
events, err := journal.Replay(file)
t, err := terms.Fold(events)       // milestones, total, cap, evidence strength
a := acceptance.Fold(events)       // windows, rejections, holds, refusals
n, err := nte.Fold(events)         // cap in force, minutes logged, notices
f, err := billable.Read(events)    // everything an invoice would rest on
out, err := billable.Derive(f)
```

## What the journal never does

- **This is not legal advice.** It is a program that folds a file. Whether the terms it records match the contract you signed, and whether that contract is enforceable where you are, are questions for a lawyer.
- **Contract semantics are configuration.** The window length, the retest rule, the calendar, the weekend, the holidays, the closing time, the cap and the rate all come out of `contract.opened` and the change orders that follow it. The library has no opinion about what they should be; it only enforces that they are stated and that they do not move without the moves being recorded.
- **Approvals are evidence records, not electronic signatures.** An `ed25519` approval is a cryptographic record that a particular key signed particular bytes. It is not an electronic signature under ESIGN, UETA or eIDAS, it is not a qualified or advanced signature, and nothing here performs signer identity verification, consent capture or long-term signature validation. If a jurisdiction or a counterparty requires a legally recognised signature, obtain it in whatever system does that, and record its SHA-256 as a `recorded` approval.
- **B2B engagements only.** The mechanisms here — deemed acceptance, not-to-exceed caps, mutual holds — assume two businesses negotiating at arm's length. Consumer contracts are subject to protections this library neither knows nor implements, and deemed acceptance in particular is often unenforceable against a consumer.
- **It stops before invoicing.** A billable event carries a kind, an amount in minor units and the currency. No tax rate, no tax amount, no invoice number, no buyer tax id, no rounding rule, no payment terms, no late-payment interest, no dunning. Those belong to an invoicing system and to the tax rules of a specific jurisdiction, and a config that tries to set them here is refused.

## Related work

Read for ideas while designing this; no code was taken from either.

- [Accord Project cicero-template-library](https://github.com/accordproject/cicero-template-library) (Apache-2.0) — a library of contract clause templates with machine-readable parameters. The idea borrowed is the separation between the clause and its parameters: here, the acceptance window, the calendar and the cap are data in `contract.opened` rather than constants in the fold, which is what lets a replay tell which terms were in force when.
- [django-referral-system](https://github.com/Egor4ik325/django-referral-system) (MIT) — a referral and payout app. The idea borrowed is keying an outbound notification by a value derived from the state that triggered it, so a re-run emits nothing new; `nte.NoticeKey` hashes the contract, the cap version and the threshold for the same reason.

Maintained by [polycratia](https://polycratia.com).
