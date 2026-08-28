# Random Auto Draw and Runtime Log Implementation Plan

**Goal:** Execute one randomized automatic draw per account in each approved daily window and expose seven-day safe execution logs.

1. Extend state storage with schedule records and log records, including seven-day pruning.
2. Add an in-process scheduler that restores/generates daily plans and calls the existing draw service once per due plan.
3. Wire scheduler lifecycle into the web server and expose a read-only logs API.
4. Add a compact runtime-log panel to the workbench.
5. Run focused, full, race, static, and startup smoke checks.
