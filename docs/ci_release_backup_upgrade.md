# CI, Release, Backup and Upgrade

## Pull request CI

Run:

```text
Go format check
Go vet
Go unit tests
race tests where practical
frontend lint
frontend type check
frontend tests
SQL migration test
cache isolation integration test
fake PMS proxy integration test
Docker image build
Compose smoke test
security scan
```

## Required protocol fixtures

Keep sanitized fixtures for XML and JSON metadata, playback decision, Continue Watching, timeline, progressive part requests, HLS manifest, DASH manifest when available and PMS events.

## Release images

Publish versioned `linux/amd64` and `linux/arm64` images.

Use canonical image naming:

```text
ghcr.io/<owner>/replx-edge:<version>
```

## Backup

Backup PostgreSQL and the secret material required to decrypt owner credentials.

Do not require backup of Valkey or artwork cache.

## Restore test

Automated release testing should restore a backup into a clean stack and verify server identity, owner auth metadata, users, policies, client compatibility and configuration.

## Migration

Migrations are forward versioned and locked. Avoid destructive migrations where possible.

## Upgrade

```text
docker compose pull
docker compose up -d
```

## Rollback

Application rollback is supported only where the database migration notes say the previous binary remains compatible.
