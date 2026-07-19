# Fact Representation Framework for a Conversational Knowledge Graph

*How to decide where each kind of fact physically lives when an assistant stores what it
learns in a property graph (e.g. Apache AGE / openCypher), and how to canonicalize values
before they land there. This is the reusable **method**; the concrete predicate
vocabulary and per-predicate rulings are application-specific and belong in the consuming
application, not here.*

Companion to `fact-database-research.md`.

---

## 1. The four representations

For a property graph, a fact can be encoded four ways, increasing in weight and in what
they buy you:

| # | Representation | Shape | Buys you | Costs |
|---|---|---|---|---|
| 1 | **Attribute** | `(:Person {birthday:'…'})` | Simplest; one lookup | No provenance/history; single-valued |
| 2 | **Edge** (optionally temporal) | `(:Person)-[:SPOUSE_OF]->(:Person)` | Traversable relationship; edge can carry `valid_until` | Binary only; light provenance |
| 3 | **Entity promotion** | `(:Person)-[:HAS_ACTIVITY]->(:Activity {…cluster…})` | A fact-cluster gets one home node; n attributes hang off it | An extra node to dedup/resolve |
| 4 | **Reified statement** | `(:Person)-[:SUBJECT_OF]->(:Statement)-[:ABOUT]->(x)` | Full per-assertion provenance, valid-time, supersession, n-ary | Heaviest; most nodes/edges |

**Entity promotion (#3) is the workhorse for anything cluster-shaped.** A scheduled
activity, for instance, is not one fact but a bundle (end time, deadline, recurrence,
policy); promoting it to a node gives the whole bundle one home. That demotes full reified
statements (#4) to a rare escape hatch — used only when a fact needs an audit trail or is
irreducibly n-ary with no natural host entity.

---

## 2. Decision criteria (the rule)

Pick the **lightest representation that satisfies the fact's actual needs**, judged on:

1. **Needs provenance / an audit trail** (who asserted it; may be contested/revised with
   history retained)? → lean **reified (#4)**.
2. **Irreducibly n-ary** — 3+ parties, or its own bundle of attributes with no natural
   host? → **entity promotion (#3)**, else **reified (#4)**.
3. **A relationship between two entities you'll traverse?** → **edge (#2)** (add
   `valid_from`/`valid_until` edge properties when time-bounded).
4. **An intrinsic, single-valued trait that rarely changes and needs no history?** →
   **attribute (#1)**.

Two standing principles:

- **Don't over-reify early.** Reification is easy to add later and painful to unwind.
  Start light; promote a predicate (attr → edge → entity → reified) when a real need
  appears.
- **One definition per value.** Never store a relationship in both directions
  (`parent` *and* `child`). Pick one canonical direction and traverse it backward.

### Category guidance (generic)

| Kind of fact | Typical representation |
|---|---|
| Intrinsic single-valued traits (name, birthday, pronoun) | **attribute** |
| Contact methods (multi-valued, channel-typed) | **entity promotion** → `ContactPoint` nodes |
| Stable binary relationships | **edge** (one canonical direction) |
| Time-bounded relationships (membership, residence, employment) | **temporal edge** (validity props); retain history if the app needs it |
| Activities / events / scheduled clusters | **entity promotion** → `Activity`/`Event` node holding the cluster |
| Preferences you query across people ("who likes X?") | **edge** to a shared, deduped `Topic` node |
| Multi-party coordinations | **entity promotion** → `Arrangement` node, or **reified** |
| Provenance-critical / contested claims | **reified statement** |

The *concrete* predicate names and the per-predicate rulings for a given assistant are an
application decision — keep them in that application, not in this framework.

---

## 3. Temporal validity is an orthogonal layer

Expiration is **not** a fifth representation — it's a property layer on top of whichever
one holds the fact: a sibling attribute (`valid_until`), an edge property
(`[:MEMBER_OF {valid_until}]`), or a reified node's `valid_from`/`valid_until`/`invalidated_at`.

**Rule: never delete an expired fact — set its validity bound and keep it.** A periodic
sweep marks anything past its window invalid, preserving history for "what used to be
true" and for belief revision.

---

## 4. Value canonicalization

Constraining *predicates* (e.g. via a response-schema enum) does not touch *values* —
those are open strings. Canonicalize them deterministically **before** placement, wherever
the fact lands. The same normalizer is needed by the live write path for dedup: two
spellings of one value must collapse, or an attribute overwrite stores the wrong thing.

| value type | canonical form | example |
|---|---|---|
| time | `HH:MM` 24-hour | "4:30pm" → `16:30` |
| date (no year) | `MM-DD` | "January 11" → `01-11` |
| date (with year) | ISO `YYYY-MM-DD` | resolved against a reference date |
| pronoun | subject form | "them" → `they` |
| recurrence | iCal RRULE | "Tue & Thu" → `FREQ=WEEKLY;BYDAY=TU,TH` |
| phone | E.164 | "(555) 123-4567" → `+15551234567` |
| duration / relative | resolved absolute | "by 5pm" → `17:00` |

**Typed value slots.** Tag each extracted value with a `value_type`; the schema can then
hint format per type, the canonicalizer rewrites the value deterministically, and the
compiler places it. This matters because promoted-node attributes must be
**machine-usable** — you can't compare "5pm" as a time; you need `17:00`.

**Inject the reference time.** Relative expressions ("until Christmas", "by 5pm") resolve
only against a current time. Pass an injected `now` into the canonicalizer; never read the
wall clock inside it.

---

## 5. How it lands (three components + provenance)

A representation decision is encoded in three places that must agree:

1. **The extraction contract** (prompt + response schema) — steer the model toward the
   chosen representation and tag `value_type`.
2. **The compiler** (structured extraction → openCypher) — branch per representation:
   attribute `SET`, labeled edge, temporal edge with validity props, entity-promotion
   node+edge, or reified statement node.
3. **Any gold/eval set** — so quality is measured against the representation you actually
   want; grade values after canonicalization and edges by canonical direction.

**Provenance without bloating the graph.** Rather than reifying every fact to carry "who
said it, when," keep an **append-only write log** in the episodic layer — one row per
write (`target, predicate, representation, value, value_type, source_ref, speaker, ts,
confidence`). The graph holds the *current* value (light); the write log answers "how do
we know / when learned / supersession history." Inline provenance on edges/reified nodes is
additive, only where a query needs it at traversal time.

---

## 6. Property-graph / AGE limitations to design around

- **No native date/temporal type** in the graph value type — store times/dates as epoch
  integers or ISO strings and compare in `WHERE`/SQL.
- **Index via the underlying tables** — create GIN/btree/expression indexes on hot
  properties (e.g. `valid_until`) using ordinary SQL.
- **openCypher subset** — keep write queries simple and idempotent; `MERGE` on stable keys
  to avoid duplicate nodes (reinforcing resolve-before-write).
- **Hybrid queries** — because the graph is just relational tables underneath, a vector
  similarity search and a Cypher traversal can be joined in one query/transaction.
