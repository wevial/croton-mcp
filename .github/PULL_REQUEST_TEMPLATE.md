## What and why

<!-- One focused change. Link the ticket or issue. -->

## How to test

```sh
go build ./... && go vet ./... && go test -race ./...
```

## Checklist

- [ ] Title and commits follow Conventional Commits
- [ ] Tests cover changed behaviour, once, at the lowest layer
- [ ] No credentials, account identifiers, mailbox content or unredacted logs
- [ ] Writes stay opt-in, Seen `UID STORE` and native `UID MOVE` only, or N/A
- [ ] `docs/MCP.md` or `docs/INSTALL.md` updated, or N/A
