// Package ports holds the engine's own interfaces between its parts that
// no module outside the engine implements or uses: the ingress and
// authentication (built-in http, jwt, keys), tracing, the hubs of
// forgotten subjects and rollout changes, the value-level stores the row
// bridge and the in-memory store implement, the request signer, the
// catalog of model interfaces, and helpers (the system clock, Nop, the
// time offset format). A port another module needs moves to the public
// port package.
package ports
