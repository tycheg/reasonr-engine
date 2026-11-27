package appapi

import (
	"io/fs"

	"github.com/tycheg/reasonr-engine/internal/registry"
)

// defaultBundle is the web runtime build served at /ui/ when ingress.ui has
// neither dir nor proxy: a binary links it from the web repository (package
// bundle, registry.RegisterUIBundle). Without one the server serves the API
// and the UI program only.
func defaultBundle() fs.FS { return registry.UIBundle() }
