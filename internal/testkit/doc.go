// Package testkit provides an in-process fake IMAP server for tests.
//
// Start listens on an ephemeral 127.0.0.1 port behind a freshly generated CA,
// over implicit TLS or STARTTLS. It never reaches Proton Mail Bridge, and every
// seed must be synthetic: reserved .test addresses and invented content only.
// Commands records each client command, refused ones included, so tests can
// assert transcripts with AssertReadOnlyCommands, AssertNoInsecureAuthentication
// and ParseTranscript.
//
// The fake has three mutually exclusive shapes:
//
//   - The default fixture serves two built-in messages. Scenario injects
//     deterministic protocol faults such as delays, disconnects, oversized
//     responses and mismatched FETCH items.
//   - Messages serves custom MIME messages at UIDs from 101, with Seen marking
//     read ones. MetadataFlags picks the FLAGS shape of metadata FETCH, and
//     BodyStructures gives each message a typed BODYSTRUCTURE tree.
//   - Stateful serves mutable mailboxes seeded from StatefulOptions. It
//     supports SELECT, EXAMINE, LIST, STATUS, UID SEARCH, UID FETCH with
//     BODY.PEEK, UID STORE of \Seen only, and UID MOVE when Move is set. Every
//     other mutation is refused without changing state. Snapshot and the
//     mailbox controls let tests witness and alter that state directly.
//
// InjectFault arms a one-shot fault on a stateful UID STORE or UID MOVE, before
// or after the fixture applies it, that drops the connection or holds the
// response, so tests can witness the outcome of an unacknowledged mutation.
//
// The fake is test infrastructure only. It does not enable mutation in Croton
// and does not describe the real Bridge mailbox layout.
package testkit
