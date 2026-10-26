
# Agent Development Guide

A file for [guiding coding agents](https://agents.md/).

## Commands

- **Nightscout instance** `docker-compose -f nightscout.compose.yml up` will run and rebuild as files change
- **Nightscout Test** `go run main.go nightscout`
- **Nighttroop Test** `go run main.go nighttroop`
- **Nocturne Test** `go run main.go nocturne`

## Directory Structure

## Issue and PR Guidelines

- Never create an Issue
- If you create a PR always start PR description with bold letters saying "I AM A LITTLE AI HERE TO HELP"
- One PR per feature or fix. When asked for several things, make separate PRs; stack a PR on the branch it depends on and say so in its description.
- A PR description says what changed and why, notes any approved size over the limit, and lists the tests.
- Remove code a change makes dead in the same PR.

## Change Size

Keep every change as small as possible.

- A single change must modify no more than 100 lines of code (not including generated code, not including test code). If a change requires more changed lines, stop and ask for approval before proceeding.
- Do not edit more than 50 files in a single change (other than generated files)

## Tests
