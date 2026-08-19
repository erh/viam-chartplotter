---
description: Implement one task card from mobile-todo.md (usage: /card A1)
---

Implement task card **$ARGUMENTS** from `mobile-todo.md`, and only that card.

## Steps

1. Read the card `$ARGUMENTS` in `mobile-todo.md`, plus the "Working agreement"
   and "Context you'll need" sections at the top of that file. Read the web
   references the card names (`file:line`) before writing any code — the web app
   is the contract for data handling.
2. Check the card's `Depends on:` line. If a dependency has not landed on this
   branch's base, say so and stop rather than implementing it too.
3. If the card depends on a `viam_sdk` symbol listed as unverified, verify it
   resolves against the pinned SDK version **first**. If it doesn't, stop and
   report — do not work around it.
4. Implement it. Stay inside the files the card's `Files:` line names; if you
   need to touch something else, note why in the PR body.
5. Port any pure logic with its tests translated from the web `.test.ts`.
6. Run `make mobile-analyze` and `make mobile-test`. Both must pass.
7. Commit on a branch named `claude/$ARGUMENTS-<short-slug>`, push, and open a
   PR whose body:
   - names the card and what changed,
   - copies the card's **Accept** checklist, ticking only what you verified
     yourself and marking the rest "needs device test",
   - flags anything you could not verify without hardware or a boat.
8. Tick the acceptance boxes you verified in `mobile-todo.md` in the same PR.

## Rules

- One card, one PR. Don't opportunistically fix neighbouring cards.
- Don't change anything under the Go module (`module.go`, `render/`, `weather/`,
  `nav*.go`) or in `src/`. Report if a card seems to need it.
- The app must still compile and run chart-only with no credentials — that's
  what CI builds.
- You cannot test on a device or against a real boat. Say plainly what you
  verified (analyze, tests, logic) and what a human still has to check on the
  water.
