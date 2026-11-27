// Package kind is the contract of engine kinds (plan 0021, decision 7;
// plan 0022, decision 3): an engine implementation chosen by `engine:` in
// engine.yaml — stateless (reasonr-stateless) or a kind of another module —
// gets the shared part of the engine as a Host and
// gives an executor and background roles. Here are also the stores at the
// level of model values, the kernel, the executor and the test driver:
// they carry the compiler's types, so they are not in the contract of
// plugins (package port), which an adapter knows alone.
package kind
