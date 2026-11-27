package port

import "io/fs"

// UIBundle is the build of the web runtime a binary serves at /ui/ when
// app.ui has no dir of its own; it registers with registry.RegisterUIBundle.
type UIBundle = fs.FS
