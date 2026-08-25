## Summary

Describe the problem and approach.

## Compatibility, migration, and security

Describe API, protocol, PostgreSQL migration, identity, authorization,
deployment, and rollback effects. Write `None` when there are none.

## Verification

List the checks performed, including relevant commands such as `make check`
and the PostgreSQL version used for integration tests.

## Checklist

- [ ] Tests cover new or changed behavior and failure paths.
- [ ] User and operator documentation plus `CHANGELOG.md` are updated when needed.
- [ ] Migration and rollback implications are documented.
- [ ] Security and authorization boundaries are documented when changed.
- [ ] No credentials, workload content, log bytes, private keys, or local state are included.
- [ ] The pull request is focused on one coherent change.
