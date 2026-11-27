package server

import (
	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/ports"
)

// uiCatalog holds the models with an interface for the app API (RFC §22)
// by the model schema — the executor gives it for a tenant.
type uiCatalog map[*reasonr.Schema]*reasonr.Model

func (c uiCatalog) UI(sc *reasonr.Schema) *reasonr.Model { return c[sc] }

func (c *common) uiCatalog() ports.UICatalog {
	cat := uiCatalog{}
	for _, mi := range c.models {
		if len(mi.model.UI().Apps) == 0 {
			continue
		}
		cat[mi.model.Schema()] = mi.model
	}
	if len(cat) == 0 {
		return nil
	}
	return cat
}
