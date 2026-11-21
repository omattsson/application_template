---
name: add-migration
description: Add a versioned database migration to the backend, with the version number, the registration, the Down, the test and the places that count migrations. Use when a change needs a new table, column or index.
argument-hint: "<what the migration does>"
disable-model-invocation: true
---

# Add a migration

Migrations live in `backend/internal/database/migrations.go`. Read the "Migrations" section of `backend/CLAUDE.md` first.

## Steps

1. **Pick the version.** `git fetch origin`, then list the versions on `origin/main` and in every open pull request that touches `migrations.go`:
   `git grep -hoE 'Version: +"[0-9]{14}"' origin/main -- backend/internal/database/migrations.go` and `gh pr list --state open --json number,title,files` (look for `migrations.go`).
   The version is `YYYYMMDDNNNNNN`: today's date and a counter, higher than every version found. Two branches that pick the same or an out-of-order version break the rollback order, so say which open pull request you saw and tell the user when one also adds a migration. Whoever merges second re-counts the lists in step 5.
2. **Write the migration** as a function `<verb><What>Migration() schema.Migration` at the end of the file, with `Version`, `Name`, `Description`, `Up` and `Down`. Both directions are idempotent. A migration that touches a table that a later model changes uses a private struct of the old shape (`legacyUser`, `sessionEntryV1`, `jobV1`), never the live model. `Down` undoes only what `Up` added (`dropTables` for a table). Never edit an applied migration.
3. **Register it**: append `migrator.AddMigration(...)` to the end of `registerMigrations`.
4. **Test it** in `<name>_migration_test.go` next to the others (see `jobs_migration_test.go`): `Up` creates the objects, `Up` twice is safe, `Down` removes only its own and leaves the other tables.
5. **Update the places that count migrations**: the rollback sequence and version lists in `migrations_test.go` (the `roll back <version>` lines) and `auth_tables_mysql_integration_test.go`. Find them with `git grep -n '<previous latest version>' backend`.
6. **Docs**: the schema is described in `docs/claude/structure.md` and `ARCHITECTURE.md`. Update what the change touches.
7. **Verify**: `cd backend && go test ./internal/database/... -short`, then `make test-backend-all` (a real MySQL run: SQLite does not catch every difference), then `make lint`.

Report the version you chose and the open pull requests that conflict with it.
