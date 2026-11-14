// Package wire is the Maestri Wire client: pairing, SPKI-pinned HTTPS/WSS, the REST reads and
// streams Maestria mirrors, and the few allowlisted writes it performs.
//
// Two tokens are held. The guest token does every read and every subscription (an owner
// subscribing to a feed wakes terminals; a guest does not). The owner token is used only for the
// writes in allow.go. Neither token is ever logged, returned by the panel's API or sent to a browser.
//
// Wire is in beta (protocolVersion 1): unknown fields and capabilities are ignored, and a missing
// capability turns its feature off. See docs/maestri-wire.md.
package wire
