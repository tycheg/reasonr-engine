package appapi

import (
	"fmt"
	"strings"

	reasonr "github.com/tycheg/reasonr-compiler"
)

var basicKinds = map[string]reasonr.Kind{
	"int": reasonr.KindInt, "bigint": reasonr.KindBigint, "float": reasonr.KindFloat, "decimal": reasonr.KindDecimal,
	"string": reasonr.KindString, "bool": reasonr.KindBool, "uuid": reasonr.KindUUID, "date": reasonr.KindDate,
	"time": reasonr.KindTime, "timestamp": reasonr.KindTimestamp, "duration": reasonr.KindDuration, "bytes": reasonr.KindBytes,
	"PageRequest": reasonr.KindPageRequest, "Timer.Id": reasonr.KindTimerID,
}

// typeOf turns a type written in the interface program ("Order.Id",
// "list<OrderRow>", "string?") into a schema type.
func typeOf(sc *reasonr.Schema, t string) (reasonr.Type, error) {
	t = strings.TrimSpace(t)
	if strings.HasSuffix(t, "?") {
		inner, err := typeOf(sc, strings.TrimSuffix(t, "?"))
		inner.Optional = true
		return inner, err
	}
	if k, ok := basicKinds[t]; ok {
		return reasonr.Type{Kind: k}, nil
	}
	if i := strings.IndexByte(t, '<'); i > 0 && strings.HasSuffix(t, ">") {
		args := splitArgs(t[i+1 : len(t)-1])
		elem, err := typeOf(sc, args[len(args)-1])
		if err != nil {
			return reasonr.Type{}, err
		}
		switch t[:i] {
		case "list":
			return reasonr.Type{Kind: reasonr.KindList, Elem: &elem}, nil
		case "set":
			return reasonr.Type{Kind: reasonr.KindSet, Elem: &elem}, nil
		case "page":
			return reasonr.Type{Kind: reasonr.KindPage, Elem: &elem}, nil
		case "map":
			key, err := typeOf(sc, args[0])
			return reasonr.Type{Kind: reasonr.KindMap, Key: &key, Elem: &elem}, err
		}
	}
	if owner, ok := strings.CutSuffix(t, ".Id"); ok {
		if ti := sc.Lookup(owner); ti != nil {
			return reasonr.Type{Kind: reasonr.KindID, Ref: ti.Ref}, nil
		}
	}
	if ti := sc.Lookup(t); ti != nil {
		if ti.Kind == reasonr.TypeEnum {
			return reasonr.Type{Kind: reasonr.KindEnum, Ref: ti.Ref}, nil
		}
		return reasonr.Type{Kind: reasonr.KindRecord, Ref: ti.Ref}, nil
	}
	return reasonr.Type{}, fmt.Errorf("appapi: unknown type %q", t)
}

func splitArgs(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			depth++
		case '>':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}
