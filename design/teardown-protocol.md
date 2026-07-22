# Teardown protocol: stop → drain → close

This note states the protocol the engine uses to tear down a producer → queue →
consumer pipeline that also owns close actions (flushing a file, writing a
sidecar), and proves why it is correct. It generalizes the specific fix in
AATK-15 (the Twilio data-plane tap), but the reasoning applies to any teardown of
an asynchronous single-producer pipeline in this codebase.

## The shape of the problem

A live call wires three roles around a bounded queue `Q`:

- **Producer** — one goroutine (the `handleStream` read loop) that decodes
  frames off the WebSocket and `Send`s media frames into the data plane `Q`
  (`dropOldestPlane`).
- **Consumer** — one goroutine (`pumpDataPlane`) that `Recv`s each frame,
  records it to the tap, and forwards it to the session.
- **Close actions** — `tap.Close()` (flush writers, emit the sidecar) plus
  `sess.Close()`.

"Done" is the in-band Twilio `stop` frame (or a socket read error / context
cancel). When it arrives, the read loop returns and the teardown runs.

### The bug this protocol fixes

The naive teardown cancels the consumer's context and joins the goroutine, then
closes the tap. But cancelling the context does **not** drain `Q`: the
consumer's `Recv` is a `select` over a ready `<-ch` and a ready `<-ctx.Done()`,
which Go resolves **50/50**. A media frame still buffered in `Q` at cancel time
can therefore be abandoned — lost from *both* the tap recording and the session
delivery. Under load the cancel branch wins often enough to flake
(`TestTap_WiredToDataPlane`). It is an ordering (happens-before) gap, not a data
race — `-race` is clean and its slowdown actually masks it.

## The protocol

On teardown, in this order, each step a **completed barrier** before the next:

1. **Stop input.** Signal the producer to stop **and wait until it has
   quiesced** — every in-flight `Send` has returned and no future `Send` is
   possible. A signal alone is insufficient; you need the acknowledgement.
2. **Drain.** The consumer keeps receiving until `Q` is empty.
3. **Close.** Run the close actions.

The ordering is forced — every adjacent swap breaks a property:

- Drain before stopping input → "empty" is not a stable state; the producer
  refills behind the drainer, so drain has no defined completion.
- Close before drain → buffered items are lost/truncated (the bug above).

## Why it is correct

Let `Q` be the queue, `C` the consumer, `R` the resources closed in step 3.

**Termination (liveness).** After the step-1 barrier, `|Q|` is fixed and cannot
grow (no producer can `Send`). `Q` is finite, so `C` empties it in ≤ `|Q|`
receives, each making progress. Drain terminates. This is *why* step 1 must be a
completed join, not a bare signal: if a producer could still `Send`, `|Q|` could
grow indefinitely and drain need never finish.

**Completeness (no lost frame).** Take any item `x` sent before the stop.
Because the stop is **in-band** — a marker the same loop reads *after* all prior
media frames — the producer's `Send(x)` returns before the producer observes the
stop and quiesces. The step-1 barrier happens-after that quiesce (the join),
which happens-after `Send(x)`. So when drain begins, `x` is in `Q` or already
consumed; drain empties `Q`, so `C` processes `x`; `C`'s processing
happens-before the drain-complete barrier, which happens-before close. Therefore
every pre-stop item is processed before close. ∎

**Safety (no use-after-close, no panic).** Close runs only after the
drain-complete barrier, hence after the producer has quiesced. At that instant no
producer exists and `C` is past its last access to `Q`, so close has exclusive
access to `R` — no concurrent `Send`/`Recv`/process, no send-on-closed-channel,
and an idempotency guard prevents double-close. ∎

## The clean realization: the closed channel is the marker

The tidiest way to satisfy steps 1 and 2 together is to make the **producer close
the queue's channel** as its last act, and have the consumer **drain until
closed**:

```
producer:  … Send(x1); Send(x2); …; Q.Close()      // close = "mark where Done was"
consumer:  for { f, err := Q.Recv(ctx); if err == errPlaneClosed { return }; process(f) }
teardown:  Q.Close(); joinConsumer(); closeActions()
```

`close(ch)` happens-after every `Send` (same goroutine, program order), and a
closed channel delivers every buffered item before it reports closed. So `Recv`
returns each buffered frame in turn and then the `errPlaneClosed` sentinel once
the channel is closed **and** empty — the consumer processes every sent item,
then exits. That single edge gives you *both* barriers, with no
context-vs-buffer race to lose. The closed channel is exactly the boundary
marker: "everything up to Done, and nothing after."

Crucially, the consumer's **graceful** termination is driven by the channel
close, **not** by cancelling the consumer's context. The context's `Done` case
remains only as a hard-abort escape (see edge case 3). Because the producer
closes the channel on *every* exit path (stop frame, read error, context
cancel), the consumer always terminates.

## Edge cases — the conditions the guarantee rests on

1. **Every producer must be covered.** Multiple producers → the stop must reach
   all of them and the join must await all; miss one and `Q` can still grow. Here
   there is a single producer (the read loop), and single-producer is also what
   makes "the producer closes the channel" safe (Go's rule: only the sole/last
   sender closes). A second data producer would require a producer-WaitGroup and
   one dedicated closer.
2. **Quiesce ⊇ in-flight Sends.** "Stopped" must include "finished the `Send` it
   was mid-way through," not merely "won't start new ones." Goroutine-exit /
   `WaitGroup` provides this; a shared bool does not.
3. **In-band vs out-of-band Done.** The completeness proof needs the stop to be
   *ordered* with the data. The in-band `stop` frame is. An **out-of-band** abort
   (context cancel, socket reset) is not: teardown still terminates safely, but
   the guarantee weakens to "drain whatever is buffered" — trailing frames may be
   truncated. That is the correct, acceptable semantics for an abort; do not
   claim completeness for it.
4. **Lossy queue ≠ lossless capture.** `dropOldestPlane` evicts the oldest frame
   under backpressure. "Drain the queue" processes what *survived* the drop
   policy; frames dropped earlier under load are not resurrected at teardown.
   Teardown-completeness is orthogonal to the plane's best-effort loss.
5. **The consumer must outlive the drain.** `C`'s exit condition must be
   "closed ∧ empty," never "stop received." Closing `C` (and the resources) is
   step 3, after the drain.
6. **Per-item work during drain must not block on a torn-down downstream.**
   `pumpDataPlane` also forwards each drained frame to the session; if that could
   block on something already closed, the drain deadlocks. It is safe here because
   the session input is also drop-oldest (non-blocking) and the session is closed
   *after* the drain. "Close downstream last" and "per-item work is bounded" are
   real preconditions.
7. **Close must be idempotent / serialized.** Teardown can be triggered from
   several paths (stop, read error, context cancel, panic-recover). Guard it (a
   `closed` flag under a lock, or `sync.Once`) so exactly one teardown runs and
   `R` is not double-closed. `Tap.Close` already guards on `t.closed`.
8. **Non-blocking Send is what lets the join finish.** If `Q`'s `Send` blocked
   when full, "stop the producer" would also have to unblock a producer parked in
   `Send`, or the join deadlocks. `dropOldestPlane` drops instead of blocking, so
   this holds; a blocking queue would need the stop to also break the `Send`.

## Applying it here (AATK-15)

- `dropOldestPlane` gains an idempotent `Close()` (closes its channel under the
  existing mutex; `Send` no-ops after close), and `Recv` returns the
  `errPlaneClosed` sentinel once the channel is closed and drained.
- `handleStream`'s teardown closes the data plane on every exit path and orders
  it close-data → join pumps → `tap.Close()` → `sess.Close()`; the data pump's
  graceful termination is the channel close, not context cancellation.
- The **control** plane is intentionally left on context-cancel termination: its
  frames carry no completeness requirement (the `stop` that triggered teardown
  has already been consumed), so edge case 3's "abort semantics" are the right
  contract for it.
