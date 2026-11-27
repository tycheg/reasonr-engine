package ports

import "github.com/tycheg/reasonr-compiler"

// UICatalog returns the model with an interface (RFC §22) by its schema;
// nil — the model has no apps. The app API takes from it the interface
// program, the screen descriptions and the evaluation of layout
// expressions.
type UICatalog interface {
	UI(sc *reasonr.Schema) *reasonr.Model
}
