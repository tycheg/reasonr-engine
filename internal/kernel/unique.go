package kernel

import (
	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/ports"
)

// uniqueError is the business error from IndexInfo.Error of the violated
// index: parameters named after index fields (the last path segment) get
// the key values, others none.
func uniqueError(s *reasonr.Schema, a *reasonr.Arena, uv *ports.UniqueViolation) reasonr.BusinessError {
	ti := s.Type(uv.Type)
	if ti == nil || int(uv.Index) >= len(ti.Indexes) {
		return reasonr.BusinessError{Name: "uniqueViolation", Args: a.Record(0)}
	}
	in := &ti.Indexes[uv.Index]
	be := reasonr.BusinessError{Owner: uv.Type, Name: in.Error, Args: a.Record(0)}
	for i := range ti.Methods {
		mi := &ti.Methods[i]
		if mi.Kind != reasonr.MethodError || mi.Name != in.Error {
			continue
		}
		args := make([]reasonr.Value, len(mi.Params))
		for j, prm := range mi.Params {
			args[j] = reasonr.None
			for k, sk := range in.Keys {
				if k < len(uv.Key) && leafName(s, uv.Type, sk.Field) == prm.Name {
					args[j] = uv.Key[k]
				}
			}
		}
		be.Args = a.Record(0, args...)
	}
	return be
}

// leafName is the name of the last field of a path; "id" is the record identifier.
func leafName(s *reasonr.Schema, t reasonr.TypeRef, path reasonr.FieldPath) string {
	name := ""
	for _, f := range path {
		ti := s.Type(t)
		if ti == nil || int(f) >= len(ti.Fields) {
			return "id"
		}
		name, t = ti.Fields[f].Name, ti.Fields[f].Type.Ref
	}
	return name
}
