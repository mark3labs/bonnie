// Package wire defines the shared types for BONNIE's transport APIs.
// It does not import the executor, journal, or server channels. Kit types remain
// part of the API, so this package still depends on the public Kit SDK.
package wire

// IrohALPN identifies HTTP/1.1 over an Iroh bidirectional stream.
const IrohALPN = "bonnie/http/1"
