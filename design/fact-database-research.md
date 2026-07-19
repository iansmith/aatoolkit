# Building a Conversational Assistant's Fact Database — A Research Synthesis

*Combined report on knowledge extraction from everyday conversation and its storage in a
personal knowledge graph (Apache AGE) + vector store (pgvector).*

**Method note.** The bulk of this report is grounded in a fanned-out literature review (6 search
angles, 30 sources fetched, 140 candidate claims, 25 adversarially verified by 3-vote majority —
24 confirmed, 1 refuted). Claims carried from that pass are cited with `[source]` links. Two
dimensions you asked about — **clarifying-question generation** and **Apache-AGE-specific storage
patterns** — surfaced good sources during search but were squeezed out of the verification budget,
so recommendations there are synthesized from the found sources plus direct engineering knowledge
of the AGE 1.8.0 / pgvector 0.8.5 stack we just built. Those sections are marked
**(engineering recommendation)** where they go beyond a verified claim.

---

## 0. TL;DR — the recommended architecture

The literature converges on a single shape for a conversational assistant: an **LLM-driven
extraction-to-graph pipeline** feeding a **temporally-aware property graph**, with a **vector index
over raw utterances** sitting alongside it for fuzzy recall. Six stages:

```
utterance
   │
   ▼
① EXTRACT      LLM turns dialogue → candidate (subject, predicate, object) facts + events
   │           (multi-turn window; NOT per-utterance)
   ▼
② RESOLVE      Coreference + entity resolution BEFORE writing to the graph.
   │           "my husband" / "them" / "Erin's mother" → canonical node, or a NIL placeholder.
   ▼
③ TIME         Attach bi-temporal validity: valid-from / valid-until (world time) +
   │           ingested-at / invalidated-at (system time). "until Christmas" → valid_until.
   ▼
④ RECONCILE    Dedup against existing facts; detect contradictions; supersede (don't delete)
   │           stale facts; record provenance (who said it, when, confidence).
   ▼
⑤ STORE        Write to Apache AGE as a property graph. Embed the raw chunk into pgvector,
   │           linked back to the graph nodes it produced.
   ▼
⑥ RETRIEVE     Hybrid: pgvector similarity finds candidate memories → AGE Cypher traversal
   & GAP-FILL  resolves entities/time. Unfilled NIL placeholders + missing attributes seed
               clarifying follow-up questions.
```

The single closest published blueprint to what you're building is **Zep / Graphiti** — a
temporally-aware knowledge-graph memory engine with bi-temporal edges. Treat it as the reference
design and AGE as the storage substrate.

---

## 1. Fact extraction from conversation — the hard part

**Conversational input is materially harder than the encyclopedic text most extractors were built
for.** Triple-extraction methods were developed and tuned for Knowledge Base Completion on
Wikipedia text; social conversation differs by genre (statements / questions / answers) and
foregrounds **co-reference, ellipsis, coordination, and negation**. The empirical hit is dramatic:
best *complete-triple* precision was ~**51%** at the single-turn level but collapsed to ~**3%** when
triples span multiple turns (mBERT).
[[arxiv 2412.18364](https://arxiv.org/html/2412.18364)]

A dedicated task, **Open Knowledge Extraction from Dialogue (OKE-D)**, formalizes exactly the assistant's
job: take a sequence of *speaker-annotated* utterances and produce linked `(s, p, o)` triples.
Across five evaluated extractors (a hand-written CFG, spaCy dependency parsing, Stanford OpenIE,
fine-tuned mBERT/Albert with IOB tagging, and few-shot Llama 3.2 emitting JSON), the simple **CFG**
and **mBERT** were strongest. [[CEUR Vol-4085 paper25](https://ceur-ws.org/Vol-4085/paper25.pdf)]

**Implication for the assistant:** don't run a naive per-utterance extractor. Feed a **multi-turn context
window** and resolve coreference first (§2).

### How to prompt the LLM extractor

Verified recipes that lift extraction quality:

- **QA-mediated extraction (SocraticKG):** rather than extracting triples directly, generate and
  answer **5W1H questions** ("who has a birthday? when? whose?") about an utterance, then convert
  the QA pairs into triples. [[arxiv 2601.10003](https://arxiv.org/pdf/2601.10003)] This maps
  naturally onto the assistant because she's *already* a conversational agent — the questions she'd ask to
  fill gaps (§6) are the same questions that drive extraction.
- **Extract-Critique-Refine (ECR):** a 3-step prompt (extract → self-critique → refine) emitting
  entities + relations + triples gave **+16% triple-F1** over a one-shot baseline; a Chain-of-Thought
  "extract only triples" prompt hit the best single triple-F1 of **0.73**.
  [[arxiv 2506.19773](https://arxiv.org/pdf/2506.19773)]
- **Automatic prompt optimization** (DSPy, APE, TextGrad) produced human-comparable extraction
  prompts, with the **largest gains as text size, context length, and schema complexity grow** —
  relevant once the assistant's ontology is rich. [[arxiv 2506.19773](https://arxiv.org/pdf/2506.19773)]

**One important tradeoff, flagged:** a mid-2023 finding held that GPT-4-class models are "more
suited as inference assistants rather than few-shot information extractors"
[[arxiv 2305.13168](https://arxiv.org/abs/2305.13168)] — i.e. lean on the LLM to *reason over* the
graph at query time, and use a more structured/verified path for *writing* to it. LLM extraction
has matured a lot since; treat this as a design bias (verify what you write), not a hard law.

---

## 2. Coreference & entity resolution — the load-bearing stage

the assistant's facts are only meaningful when linked to the right person. "My husband's birthday is
Aug 8" is useless as a floating triple; it has to attach to a stable *husband* node. This stage is
where a personal KG lives or dies.

- **Standard document entity-linking is suboptimal for conversation.** Personal entities
  ("my cars", "my husband") and concepts are first-class in dialogue and absent from a public KB.
  The **CREL** conversational-entity-linking toolkit adapts coreference resolution to link
  possessive/pronoun personal-entity mentions to their explicit antecedents in the dialogue.
  [[arxiv 2206.07836](https://arxiv.org/pdf/2206.07836)]
- **Resolve coreference BEFORE building the graph.** LINK-KG's three-stage pipeline (NER-LLM →
  Mapping-LLM → Resolve-LLM, with a type-specific "Prompt Cache" tracking references across chunks)
  cut duplicate entity nodes by **45%** on average (up to **61%** vs. GraphRAG). Canonicalize
  entities up front or you'll get three different "husband" nodes.
  [[arxiv 2510.26486](https://arxiv.org/html/2510.26486v1)] *(Caveat: measured on legal documents;
  the magnitude is an extrapolation to short voice utterances.)*
- **Joint modeling beats a pipeline.** End-to-end joint NER + coreference + relation extraction
  over a whole document outperforms separate stages.
  [[arxiv 2107.02286](https://arxiv.org/abs/2107.02286)]

### Unknown entities are first-class: NIL / placeholder nodes

This is the mechanism for "Erin", "Erin's mother", and an as-yet-unnamed husband. Entity linking is
formally *matching a mention to a KB entry **or returning NIL** when none exists* — and real input
"virtually guarantees many entities will not appear in the KB."
[[Dredze et al., COLING 2010](https://www.cs.jhu.edu/~mdredze/publications/entity_linking_coling.pdf)]

- Learn NIL by adding a **NIL candidate into the ranker's candidate set** (with its own features),
  rather than hand-tuning a similarity threshold or bolting on a separate classifier.
  [[Dredze et al.](https://www.cs.jhu.edu/~mdredze/publications/entity_linking_coling.pdf)]
- **New/emerging entities are the hardest case** — up to **17.9%** accuracy drop for new entities
  vs. **3.1%** for continually-existing ones (TempEL).
  [[arxiv 2302.02500](https://arxiv.org/pdf/2302.02500)]

**Implication for the assistant:** when a mention doesn't resolve, **create an explicit unlinked
placeholder node** — `(:Person {name:'Erin', status:'unresolved'})`,
`(:Person {rel:'mother_of:Erin', status:'unresolved'})` — that a later clarifying answer can fill
and canonicalize. These placeholders *are* your to-do list of questions to ask (§6).

---

## 3. Temporal facts that expire — bi-temporal modeling

The choir rehearsal "until Christmas" and soccer "until 4:30pm" are facts with **validity windows**.
Adopt a **bi-temporal** model in the style of **Zep / Graphiti**, whose Graphiti engine tracks
**four timestamps per edge**:

| timestamp | meaning |
|---|---|
| `t_valid` | when the fact became true *in the world* |
| `t_invalid` | when it stopped being true in the world (e.g. Christmas) |
| `t_created` | when the assistant *ingested/learned* it |
| `t_expired` | when the assistant *marked it superseded* |

Zep uses Graphiti as its core memory to synthesize both unstructured conversation and structured
data **while preserving historical relationships** — and reports, on the LongMemEval benchmark,
accuracy improvements up to **18.5%** and **90%** latency reduction vs. baselines, being
"particularly effective for cross-session information synthesis and long-term context maintenance."
[[arxiv 2501.13956](https://arxiv.org/abs/2501.13956)] *(Confidence: medium — these are
vendor-reported numbers on the authors' own benchmark. The architecture is sound and widely cited;
the specific figures aren't independently replicated.)*

**Core rule: never delete an expired fact — mark it invalid.** Set `t_invalid` / `t_expired` and
keep the node. That preserves the history you need for "when did she used to have choir?" and for
belief revision (§4).

**Recurring schedules** (soccer Tue/Thu, choir every Monday) are best modeled as a **recurrence
rule** on an event node rather than materializing every instance — an RRULE-style string
(`FREQ=WEEKLY;BYDAY=TU,TH;UNTIL=<date>`) plus `valid_until`. Materialize concrete instances lazily
only when needed (e.g., to answer "is there soccer this Thursday?"). *(Engineering recommendation —
the RRULE-vs-materialize choice was flagged as an open question by the research; iCalendar RFC 5545
RRULE is the standard vocabulary.)*

---

## 4. Reconciliation — dedup, contradiction, belief revision, provenance

**Record who asserted each fact and when, and keep contradictory assertions as distinct
perspectives rather than flattening them.** The OKE-D work proposes a **perspective-aware dialogue
ontology** built on **PROV-O** (provenance) + **SIO**, attaching speaker identity and temporal
context to every assertion — arguing a flat KG becomes ambiguous when two speakers assert conflicting
claims or one speaker revises a belief over time.
[[CEUR Vol-4085 paper25](https://ceur-ws.org/Vol-4085/paper25.pdf)] *(Confidence: medium — a
doctoral-consortium proposal, though PROV-O provenance itself is well-established.)*

Concretely, this argues for **reifying each assertion as its own node** (a "statement" or "fact"
node) carrying `speaker`, `timestamp`, `confidence`, `source_utterance_id`. That single choice buys
you:

- **Belief revision by supersession** — a new fact points a `SUPERSEDES` edge at the old one; the
  old one gets `t_invalid` set but stays queryable. (Consistent with Zep/Graphiti's "maintain
  historical relationships" principle.)
- **Contradiction detection** — two active statement-nodes with the same `(subject, predicate)` but
  different objects and overlapping validity = a conflict to resolve (confidence-weighted, or by
  asking the user).
- **Provenance & trust** — every fact traces back to the exact utterance that produced it (and, via
  pgvector, the embedding of that utterance).

*(Note: automated write-time contradiction detection + confidence-weighted supersession beyond this
"keep perspectives distinct" principle was an explicit open question — no single algorithm won.
Graphiti's approach: an LLM compares each new edge against semantically-related existing edges and
sets invalidation. That LLM-judge-at-write-time pattern is the pragmatic default.)*

---

## 5. LLM-agent memory architectures — the named systems

Build cross-session memory on established blueprints rather than from scratch:

| System | Core idea | What the assistant borrows |
|---|---|---|
| **Zep / Graphiti** [[2501.13956](https://arxiv.org/abs/2501.13956)] | Temporally-aware KG with bi-temporal edges; LLM-driven fact invalidation | **The reference design.** Bi-temporal schema, supersede-don't-delete, cross-session recall |
| **MemGPT / Letta** [[2310.08560](https://arxiv.org/abs/2310.08560)] | OS-style tiered memory: fast in-context "main" + slow out-of-context "external", paged in/out | How to keep a *bounded* working set in the prompt while the graph holds everything |
| **A-MEM** [[2502.12110](https://arxiv.org/abs/2502.12110)] | Zettelkasten-style dynamic linking; **memory evolution** — a new memory can update existing ones' representations | Auto-linking related facts; letting new info refine old context |
| **Mem0** (practitioner) | Extract → consolidate → vector+graph store | Popular hybrid vector+graph pattern; good engineering reference |

The episodic-vs-semantic distinction matters: **episodic** = the raw utterance/event ("on Jul 19 she
mentioned choir"), **semantic** = the distilled fact ("she sings in a choir"). the assistant wants both —
episodic in pgvector (§5a), semantic in AGE.

*(One A-MEM sub-claim about its exact structured note schema was **refuted** 1-2 in verification, so
don't rely on A-MEM's specific note format — just the linking + evolution concepts.)*

### 5a. The pgvector + AGE hybrid (your addition)

Having both stores in **one Postgres instance** is a genuine advantage — one transaction, and you
can `JOIN` a pgvector similarity search against an AGE Cypher traversal with no cross-store
consistency problem. Recommended division of labor *(engineering recommendation)*:

- **Embed the raw utterance chunks** into pgvector — this is episodic memory and fuzzy recall
  ("what did she say about pickups?"). Optionally *also* embed the natural-language rendering of each
  extracted fact ("Her older child has soccer on Tuesdays and Thursdays") for semantic recall.
- **Link every chunk back to the graph** it produced. Keep a plain relational table:
  `utterance(id, ts, speaker, text, embedding vector(N))`, and store that `utterance.id` as the
  `source_utterance_id` property on each AGE statement node. That's your provenance bridge.
- **Retrieve in two hops:** (1) pgvector kNN finds the top-k relevant chunks/facts; (2) their
  `source_utterance_id`s / entity ids seed an AGE Cypher traversal that pulls the linked entities,
  their current-valid facts, and neighbors. This is the "vector-recall → graph-expand" pattern used
  by Graphiti and Mem0.

---

## 6. Knowledge gaps → clarifying questions

Every unfilled NIL placeholder (§2) and every missing expected attribute is a question to ask. The
research surfaced concrete approaches *(these sources were found but fell outside the verified-25
budget — treat as directional)*:

- **Frame gap-filling as knowledge-graph completion.** Select which question to ask by asking which
  answer would most complete the graph; a KG-completion-based question-selection method beat two
  baselines. [[KG-completion question selection, Ono et al.](https://www.researchgate.net/publication/350859214)]
  For the assistant: the graph *knows* it has a `husband` node with no `name`, and an unresolved `Erin` —
  those are the highest-value questions.
- **Follow-up question generation with KG + LLM:** a Recognition → Selection (score KG nodes by
  PageRank importance + semantic similarity) → Fusion pipeline generates deeper follow-ups than the
  LLM alone. [[arxiv 2504.05801](https://arxiv.org/html/2504.05801v1)]
- **Know *when* to ask.** Poorly chosen clarifying questions hurt satisfaction more than an
  imperfect direct answer, so weigh the risk of asking vs. guessing.
  [[arxiv 2101.06327](https://arxiv.org/pdf/2101.06327)] **ACT (Action-Based Contrastive
  Self-Training)** is a DPO-based method that teaches a model *when and how* to ask in multi-turn
  dialogue. [[Google Research](https://research.google/blog/learning-to-clarify-multi-turn-conversations-with-action-based-contrastive-self-training/)]

**Pattern for the assistant:** maintain a priority queue of gaps (missing name on a known relation,
unresolved placeholder, contradictory facts, soon-to-expire facts worth confirming). Rank by
value-of-information × how naturally it fits the current conversation, and surface at most one or two
per session so she doesn't feel like an interrogation. §6.1 makes this concrete.

### 6.1 Which question to ask, and *when* — a two-part policy

Clarifying-question generation for KB completion decomposes into two nearly independent decisions:
**which** gap to probe (a generation + value-of-information problem) and **whether/when** to voice it
(a timing + interruption-cost problem). The literature has a much stronger answer for the *timing*
half than the *generation* half — which is fortunate, because timing is exactly the "don't
overwhelm the user" constraint you flagged.

#### A. Which question — gap detection, generation, and value-of-information ranking

**The graph enumerates its own gaps.** Because unresolved mentions become explicit NIL placeholder
nodes (§2) and your schema defines the expected attributes of each entity type, the *set of missing
facts is directly readable off the graph*: a `husband` node with no `name`, an unresolved `Erin`, a
`carpool` statement with no phone number. This is the KB-completion framing — **frame question
selection as "which answer would most complete the graph."** A KG-completion-based question-selection
method beat two baselines on exactly this task
[[Ono et al., 2021](https://www.researchgate.net/publication/350859214)], and patent/industry work
describes the same loop: diff the graph against a schema to get a list of missing properties, then
generate a natural-language question per `(entity, missing-property)` pair.

**Generation — three viable strategies, in increasing flexibility:**
- **Schema templates** (`"What is {husband}'s name?"`) — most reliable, zero hallucination, best for
  the cold-start phase and for factual slots.
- **KG-path + LLM fusion** — a Recognition → Selection → Fusion pipeline scores KG nodes by
  **PageRank importance × BERT semantic similarity** and feeds the top paths to an LLM, producing
  deeper, better-contextualized follow-ups than the LLM alone.
  [[arxiv 2504.05801](https://arxiv.org/html/2504.05801v1)]
- **LLM generate-and-evaluate (AGENT-CQ)** — an end-to-end framework that generates candidate
  clarifying questions with LLM prompting *and* scores them with a simulated-judgment evaluation
  stage (usefulness, clarity, non-redundancy). Useful once the assistant's ontology is rich and templates
  feel stilted. [[arxiv 2410.19692](https://arxiv.org/abs/2410.19692)]
- (The **SocraticKG** 5W1H approach from §1 doubles as a generator here — the same who/when/whose
  probes that drive extraction *are* the clarifying questions.)

**Rank by value-of-information (VOI), not recency.** A missing **name is unusually high-VOI**
because it's a *hub*: canonicalizing "husband" unlocks every dangling fact that should attach to
him, and resolving "Erin" lets the carpool arrangement and both children link up. So the ranking
key is roughly *"how many other facts/edges does answering this unblock?"* — degree of the
placeholder node plus number of pending facts waiting on it — times a confidence/importance weight.
Identity-resolving questions (who is this person?) should almost always outrank attribute questions
(what's their favorite color?).

#### B. When to ask — interruption cost is the governing constraint

This is where the evidence is strongest and where your "can't be overwhelming" requirement lives.

**The classic decision rule (Horvitz mixed-initiative, 1999):** ask **only when the expected utility
of asking is positive** — `EU(ask) = benefit(answer) − cost(interruption)` — and otherwise stay
silent, defer, or infer. Reason explicitly about interruption cost, the expected value of the
information, and the benefit of leaving the user in control. This is the foundational framing every
modern proactive-agent paper builds on. [[surveyed here](https://arxiv.org/pdf/2601.04461)]

**Timing dominates content.** In proactive-assistance studies, **timing accounted for ~40% of the
variance in whether an intervention was accepted**, and *identical* suggestions were accepted about
**3× more often** at moments users judged appropriate vs. inappropriate. Interruption research finds
**task breakpoints / natural pauses predict low-disruption timing better than urgency does** — so
the assistant should ask at conversational seams (end of a topic, a lull), not the instant a gap appears.

**But VOI decays, so "wait forever" is also wrong.** The "Ask Early, Ask Late, Ask Right" analysis
shows the *value* of a clarification **decays over the interaction** and the decay rate depends on
the information type: goal/identity-critical clarifications lose almost all value if deferred, while
input/attribute details tolerate more delay.
[[arxiv 2605.07937](https://arxiv.org/html/2605.07937)] The practical reading for the assistant: **the
moment a fact is mentioned is when its clarifying question is both highest-VOI *and* most natural**
(the context is fresh) — so identity-resolving follow-ups belong **in-conversation, immediately**,
while low-VOI attribute gaps can be banked for later.

**Be concrete, never generic.** Users consistently prefer concrete, actionable prompts over generic
"do you need help?" openers, which read as unnecessary interruptions.
[[LlamaPIE / proactive-assistant studies](https://arxiv.org/pdf/2505.04066)] So:
*"You mentioned your husband's birthday — what's his name, so I can keep track?"* ✓, not
*"Want to tell me more about your family?"* ✗.

#### C. the assistant's two channels — mapping the policy onto her capabilities

You noted the assistant can both **ask follow-ups in-conversation** and **send unsolicited questions**.
These have very different interruption costs and should run under different budgets:

| | **Reactive follow-up** (in-conversation) | **Unsolicited push** (out-of-band) |
|---|---|---|
| Interruption cost | Low — she's already talking to the user | High — she's initiating contact |
| Best for | Highest-VOI gap created *this turn* (esp. identity resolution), asked while context is fresh | Banked high-VOI gaps, asked at a good moment |
| VOI timing | Ask **now** — VOI and naturalness both peak at mention | Batch, pick the single best, wait for a breakpoint |
| Rate budget | ~1–2 per session, fewer early on | Near-zero at cold-start; grows with tenure/trust |
| Bar to clear | "Is this the top gap and a natural seam?" | "Is `benefit − interruption_cost` clearly positive?" |

#### D. Cold-start pacing (your explicit concern)

Early in the relationship the interruption budget should be **near zero for unsolicited questions**
and **low even for in-conversation follow-ups** — Horvitz's cost term is *high* before trust is
established, and a barrage of questions on day one is the fastest way to make the assistant feel like a
form. Concrete policy:

1. **Prefer capture over probing.** Store what's volunteered, mint placeholder nodes freely, and let
   the **gap queue accumulate silently** rather than draining it immediately.
2. **At cold-start, ask at most the single highest-VOI question per session, and only when the
   conversation naturally invites it** (the user pauses, or is already on-topic). Often the right
   count is zero.
3. **Batch unsolicited questions** — never fire one the instant a gap appears. Hold them, and only
   push when (a) enough high-VOI gaps have accumulated to be worth the interruption, (b) it's a good
   time of day / natural breakpoint, and (c) the per-period rate budget allows.
4. **Grow the budget with tenure and observed receptiveness.** If the user answers readily and
   engages, raise the rate; if they ignore or deflect, back off. (This is the mixed-initiative
   feedback loop, and the ACT / risk-aware line of work
   [[ACT](https://research.google/blog/learning-to-clarify-multi-turn-conversations-with-action-based-contrastive-self-training/),
   [risk-aware clarify-vs-answer](https://arxiv.org/pdf/2101.06327)] is where to look for learning
   *when* to ask rather than hand-tuning it.)
5. **One question at a time.** Even when three facts are missing from one statement, ask for the
   single highest-VOI slot and let the rest stay queued — stacked questions read as interrogation.

**Net policy in one line:** *the graph tells the assistant **what** to ask (VOI-ranked gaps), and
interruption-cost economics tell her **whether and when** — reactively and immediately for the
top identity gap while context is fresh, and sparingly, batched, and trust-gated for everything
pushed out of band.*

---

## 7. Storing it in Apache AGE — concrete schema

*(Engineering recommendation, grounded in the AGE 1.8.0 / openCypher stack we built. AGE-specific
patterns were an open question in the research — this reflects how AGE actually behaves.)*

### 7.1 Node & edge model (reified facts)

Use a **reified statement node** per assertion so every fact carries its own time + provenance.

```cypher
-- People (canonical or placeholder)
(:Person {id, name, status:'resolved'|'unresolved', pronouns, ...})

-- A reified fact/assertion
(:Statement {
    id,
    predicate:      'birthday' | 'has_activity' | 'prefers_pronoun' | ...,
    value:          'Aug 8',           -- the object, when it's a literal
    valid_from:     1723075200,        -- epoch seconds (see 7.3)
    valid_until:    1735084800,        -- null = open-ended
    ingested_at:    1784446089,
    invalidated_at: null,              -- set on supersession, never DELETE
    confidence:     0.9,
    speaker:        'user:linda',
    source_utterance_id: 4823          -- bridge to pgvector row
})

-- Edges wire subject → statement → object
(:Person)-[:SUBJECT_OF]->(:Statement)
(:Statement)-[:ABOUT]->(:Person|:Event|:Activity)   -- object, when it's an entity
(:Statement)-[:SUPERSEDES]->(:Statement)            -- belief revision
```

Recurring events get an event node with an RRULE:

```cypher
(:Event {
    kind:'soccer_practice',
    rrule:'FREQ=WEEKLY;BYDAY=TU,TH',
    end_time:'16:30',
    hard_pickup_by:'17:00',
    valid_until: null
})
```

### 7.2 Why reification (vs. edge properties)

AGE *does* let you put properties directly on an edge (`(a)-[:BIRTHDAY {date:'Aug 8'}]->(b)`), which
is lighter. Use edge-properties for simple, single-valued, rarely-revised facts; use **reified
Statement nodes** when you need per-fact provenance, supersession history, or n-ary facts (the
pickup arrangement involves the kid, the 5pm deadline, *and* Erin's mother — that's n-ary and won't
fit on one edge cleanly). The property-graph-vs-RDF/reification tradeoff was flagged as
under-evidenced; the pragmatic rule above is standard practice.

### 7.3 AGE limitations to design around

- **No native date/temporal type in `agtype`.** AGE's value type has numbers, strings, booleans,
  lists, and maps — *not* a first-class timestamp. **Store times as epoch integers** (best for
  range comparisons like "valid_until < now") or ISO-8601 strings, and do comparisons in Cypher
  `WHERE` or in the surrounding SQL. Don't expect Cypher temporal functions.
- **Indexing is via the underlying Postgres tables.** AGE stores each label as a real Postgres
  table; create GIN/btree indexes on the `properties` column (or expression indexes on hot
  properties like `valid_until`) using ordinary SQL for query performance.
- **openCypher subset.** `MERGE`, `MATCH`, `CREATE`, `SET` work; some newer Cypher niceties don't.
  Keep write queries simple and idempotent (use `MERGE` on stable keys to avoid duplicate nodes —
  reinforcing the resolve-before-write rule from §2).
- **Hybrid queries cross the SQL/Cypher boundary.** Because the graph is just Postgres tables, you
  can wrap a Cypher `MATCH` in SQL and `JOIN` it to a pgvector `ORDER BY embedding <-> $q LIMIT k`.
  That's the mechanism for the §5a two-hop retrieval, all in one query/transaction.

---

## 8. Worked examples — the three utterances → graph

**"My husband's birthday is Aug 8; kids' birthdays Sep 22 and Jan 11 — that's the younger one."**
- Entities: `Person{rel:spouse, status:unresolved}` (no name yet → **question: husband's name?**),
  two `Person{rel:child}` nodes; the Jan-11 child gets `birth_order:younger`.
- Facts: three `Statement{predicate:'birthday'}` nodes, each `SUBJECT_OF` its person. Open-ended
  validity (birthdays don't expire).
- Gaps queued: husband's name; both kids' names; which child is Sep 22.

**"Special choir rehearsal each Monday 7:30pm until Christmas."**
- Implicit fact: `(:Person{user})-[:MEMBER_OF]->(:Group{kind:'choir'})` — **the assistant may not have
  known she's in a choir** → optionally confirm.
- Event: `Event{kind:'choir_rehearsal', rrule:'FREQ=WEEKLY;BYDAY=MO', start:'19:30',
  valid_until:<Christmas>}`. When Christmas passes, a maintenance job sets `invalidated_at` — it
  doesn't vanish.

**"Older kid prefers 'them'. Soccer Tue/Thu until 4:30, pickup by 5 or tell them beforehand. I
coordinate with Erin's mother to pick up both kids."**
- `prefers_pronoun:'they'` on the older child (high-priority preference fact — apply it everywhere).
- `Event{soccer, rrule:BYDAY=TU,TH, end:'16:30', hard_pickup_by:'17:00',
  on_delay:'notify_child_beforehand'}` — the conditional/procedural bit becomes a property.
- Placeholders: `Person{name:'Erin', status:unresolved}`,
  `Person{rel:'mother_of:Erin', status:unresolved}` → **questions: who is Erin? her mother's name &
  phone? how do I notify the kid if late?**
- A carpool arrangement is n-ary → a reified `Statement{predicate:'carpool'}` linking the user,
  Erin's mother, and both kids.

---

## 9. Caveats & open questions (from the verification pass)

- **Single/vendor-sourced claims:** Zep's 18.5%/90% numbers are self-reported on the authors' own
  benchmark; the perspective-aware OKE-D ontology is a proposal, not validated results; LINK-KG's
  45–61% dedup was on legal docs, not voice.
- **Time-sensitivity:** the "GPT-4 is a better reasoner than extractor" finding is mid-2023 —
  treat as a design bias, not law; modern LLM extraction is much stronger.
- **Genuinely open (no algorithm won):**
  1. Best openCypher/AGE pattern for bi-temporal validity + recurrence + provenance (edge-props vs
     reification vs event nodes) — §7 is the pragmatic recommendation, not a proven optimum.
  2. RRULE vs. materialized instances for recurring+expiring schedules.
  3. Best automated write-time contradiction detection + confidence-weighted supersession — the
     LLM-judge-at-write-time pattern (Graphiti) is the current pragmatic default.
- **Now addressed (see §6.1):** clarifying-question generation for KB completion. The *timing* half
  (interruption-cost economics, VOI decay, cold-start pacing) is well-evidenced; the *generation*
  half has several viable methods (schema templates → KG-path+LLM fusion → AGENT-CQ) but no single
  proven winner, so treat generation as a design choice and timing as the governed constraint.

---

## 10. Source list

**Primary (verified):**
- Zep: A Temporal KG Architecture for Agent Memory — https://arxiv.org/abs/2501.13956
- MemGPT — https://arxiv.org/abs/2310.08560
- A-MEM — https://arxiv.org/abs/2502.12110
- Extracting Triples from Dialogues — https://arxiv.org/html/2412.18364
- Open Knowledge Extraction from Dialogue (OKE-D) — https://ceur-ws.org/Vol-4085/paper25.pdf
- LLMs for KG construction/reasoning — https://arxiv.org/abs/2305.13168
- SocraticKG (QA-mediated extraction) — https://arxiv.org/pdf/2601.10003
- Extract-Critique-Refine + prompt optimization — https://arxiv.org/pdf/2506.19773
- Personal Entity Linking in Conversations (CREL/PEL) — https://arxiv.org/pdf/2206.07836
- Joint NER + coref + relation extraction — https://arxiv.org/abs/2107.02286
- LINK-KG (coref-before-KG) — https://arxiv.org/html/2510.26486v1
- Entity Linking with NIL (Dredze et al., COLING 2010) — https://www.cs.jhu.edu/~mdredze/publications/entity_linking_coling.pdf
- TempEL (temporal entity linking) — https://arxiv.org/pdf/2302.02500

**Clarifying questions — generation, selection & timing (§6 / §6.1):**
- Follow-up question generation w/ KG+LLM (PageRank×semantic scoring) — https://arxiv.org/html/2504.05801v1
- KG-completion-based question selection — https://www.researchgate.net/publication/350859214
- AGENT-CQ (LLM generate + evaluate clarifying questions) — https://arxiv.org/abs/2410.19692
- Ask Early, Ask Late, Ask Right (VOI decay / clarification timing) — https://arxiv.org/html/2605.07937
- Users mispredict preferences for AI assistance (timing = ~40% of acceptance variance; Horvitz framing) — https://arxiv.org/pdf/2601.04461
- LlamaPIE — proactive in-ear assistant (concrete > generic prompts) — https://arxiv.org/pdf/2505.04066
- Learning KGs for QA (KNOWBOT) — https://www.semanticscholar.org/paper/3aa372484e7480eefa061277cd53d49172738915
- ACT: learning to clarify (when/how to ask) — https://research.google/blog/learning-to-clarify-multi-turn-conversations-with-action-based-contrastive-self-training/
- Risk-aware clarify-vs-answer — https://arxiv.org/pdf/2101.06327

**Apache AGE / property-graph modeling:**
- Apache AGE overview — https://age.apache.org/overview/
- Property graph vs RDF — https://www.puppygraph.com/blog/property-graph-vs-rdf
- Valid-time temporal property graph — https://www.emergentmind.com/topics/valid-time-temporal-property-graph
- Graphiti KG memory (Neo4j) — https://neo4j.com/blog/developer/graphiti-knowledge-graph-memory/
