# Team and Roles

- **Team name:** AnTelOpe
- **Team lead:** Haidong Yi, [HaidYi](https://github.com/haidyi).
- **Communication channel:** [Team-5](https://stjudebiohackathon.slack.com/archives/C0BSA3FBJMB).
- **Project question/problem:** Extend and demo-test AnTelOpe (an existing AI-augmented Nextflow pipeline platform) against a real pipeline end to end.
- **Expected output:** A live demo of the golden path (register pipeline → launch job → watch live logs → ask the AI agent), plus a few small, reviewed improvements to the codebase.
- **Tools and stack:** Go 1.25 + Gin, Vue 3 + Vite + Naive UI, PostgreSQL, Redis, HashiCorp Nomad, MinIO/S3, Nextflow, Docker Compose. See the [Project Profile](../README.md#project-profile) for details.

## Roles

The roles below are suggested to match the six small first tasks in [project-plan.md](project-plan.md) — pick the ones that fit each person's comfort level with the stack, and merge or drop rows as the team size requires. Nobody needs deep Go/Vue experience for any of these; they're all scoped to avoid touching auth, job dispatch, or Nomad wiring directly.

| Person | Role | Main responsibility | Backup or support needed |
| --- | --- | --- | --- |
| [FILL: Name] | Environment & demo pipeline | Get Docker Compose running for the team; register the demo Nextflow pipeline (Tasks 1 & 3) | Some Docker familiarity helps |
| Patrick | Content & translations | Complete/align UI text in `en_US.json` -- ** 11 missing keys found in english version - updated./ `zh_CN.json` (Task 2) | None expected — JSON editing only |
| [FILL: Name] | Frontend/UI polish | Small, contained Vue/CSS improvement to one dashboard screen (Task 4) | Pairing on API shape questions if needed |
| [FILL: Name] | Backend error messages | Clarify one user-facing API error string (Task 5) | A short Go walkthrough from the team lead |
| [FILL: Name] | Docs & demo script | Write the Day 3 demo script and known-limitations doc (Task 6) | None expected |
| [FILL: Name] | Team lead | Coordinate check-ins, unblock teammates, review pull requests | — |

The tasks above is a few sample tasks you can take and the team members are also encouraged to propose their own interesting tasks to solve. 

Roles can overlap. Revisit them when the project direction or stack changes.
