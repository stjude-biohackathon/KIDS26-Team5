# Project Plan

## Goal

Get AnTelOpe (the existing AI-augmented Nextflow pipeline platform in [`src/`](../src)) running for the whole team, launch one real pipeline end to end on Nomad, and leave the researcher-facing experience a little more polished and better documented than we found it.

## Tools

Go 1.25 + Gin, Vue 3 + Vite + Pinia + Naive UI, PostgreSQL, Redis, HashiCorp Nomad, MinIO/S3, Nextflow, Docker Compose. See the [Project Profile](../README.md#project-profile) for the full list.

## First Tasks

Each task below is scoped to be doable by someone new to this stack in a few hours, and stays out of the trickier backend areas (auth, job dispatch, Nomad wiring). Claim one per person on Day 1.

- [x] Get the demo stack running locally with `docker compose up -d --build`; clarified the quick-start guide ([`src/docker/README.md`](../src/docker/README.md); the `src/docs/guide/quick-start.md` path referenced here never existed) to call out the two prerequisites that block a fresh boot: the host-side `make skills-bundle` step and a reachable local Nomad dev agent (`nomad agent -dev -bind 0.0.0.0`) - [Owner: Nick Bruggemans]
- [ ] Complete/align the UI text in `src/web_src/locales/en_US.json` and `zh_CN.json` (JSON editing only, no code logic) - [Owner: FILL]
- [ ] Register a small, public demo Nextflow pipeline in AnTelOpe and document the exact steps in a new `src/docs/demo-pipeline.md` - [Owner: FILL]
- [ ] Polish one screen under `src/web_src/src/views/dashboard/workbench` (an empty state, a tooltip, a clearer label) - [Owner: FILL]
- [ ] Rewrite one vague user-facing error message in `src/pkg/apperr` so it's clearer to a researcher hitting it - [Owner: FILL]
- [ ] Write the Day 3 demo script and a known-limitations doc; link it from the README's [Final Output and Handoff](../project-management/CHECKLIST.md#final-output-and-handoff) section - [Owner: FILL]

## Milestones

- **Day 1:** Everyone's environment runs (Docker Compose); demo pipeline/data confirmed; roles and first tasks claimed (see [team.md](team.md)).
- **Day 2:** Demo pipeline registered and launched on Nomad with live logs; small tasks in progress/review.
- **Day 3:** Golden-path demo rehearsed; demo script and known-limitations doc finished and linked from the README.

## Definition of Done

The team can run AnTelOpe locally, launch the chosen demo pipeline end to end (register → launch → watch live logs → ask the AI agent about status), and present it live with a short write-up of what worked and what didn't. If this is reached early, next steps could be adding a second, more realistic pipeline, or swapping in a bio-relevant dataset in place of the nf-core test pipeline.

## Risks and FAQs

- Nomad must be reachable from wherever the app runs — is there a shared dev Nomad cluster, or does each teammate run `nomad agent -dev` locally? Answer: We have a local nomad server that can be found at http://10.48.197.189:4646. To use it, you need to have the access to the St. Jude local network or via St. Jude VPN.
- Which demo pipeline and dataset will we use, and is it safe/licensed to show publicly? Answer: All the nf-core pipelines are publicly available w/o license.
- Most teammates are new to Go/Vue — if a claimed task turns out to need deeper changes (auth, job dispatch, Nomad), stop and ask the team lead rather than pushing through.
