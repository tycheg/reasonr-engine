package ports

import "errors"

// ErrUnauthenticated: the caller is not authenticated.
var ErrUnauthenticated = errors.New("not authenticated")

// ErrForbidden: the caller is authenticated, but the credentials do not
// give what was chosen: for example, a token for every tenant that is
// not an external system's.
var ErrForbidden = errors.New("access denied")
