// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package workspaceconfig

import (
	"fmt"
	"strings"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// GetAttribute reads the literal value of name from doc. Returns
// (value, present=true) when the attribute is set in the file;
// (zero-of-kind, present=false) when the relevant block or
// assignment is absent.
//
// Literal means: no ${env.X} interpolation. The Get/Set pair
// round-trips faithfully; interpolation is a render-time concern
// the LSP applies on its own when assembling the effective config.
//
// Errors are reserved for (a) unknown attribute names and (b)
// values present in the file but with the wrong PXF type for the
// attribute (e.g. an int where a string was expected). A file
// that simply omits the attribute is not an error.
func GetAttribute(doc *pxf.Document, name string) (value any, present bool, err error) {
	spec, ok := LookupAttribute(name)
	if !ok {
		return nil, false, fmt.Errorf("unknown attribute %q", name)
	}
	blockName, fieldName, ok := splitName(name)
	if !ok {
		return nil, false, fmt.Errorf("attribute %q: expected dotted block.field name", name)
	}
	block := findTopBlock(doc, blockName)
	if block == nil {
		return zeroFor(spec.Kind), false, nil
	}
	assign := findAssignment(block, fieldName)
	if assign == nil {
		return zeroFor(spec.Kind), false, nil
	}
	v, err := decodeValue(assign.Value, spec.Kind)
	if err != nil {
		return nil, true, fmt.Errorf("attribute %q: %w", name, err)
	}
	return v, true, nil
}

// SetAttribute mutates doc in place so name = value. Creates the
// parent block + assignment if absent; replaces the existing
// assignment's value when present. value must match the
// attribute's declared Kind (string / []string / bool).
//
// Round-trip preservation: SetAttribute only touches the target
// assignment. Every other entry — including comments, sibling
// blocks, and unrelated assignments — stays in place. The
// document is meant to be re-emitted via pxf.FormatDocument
// afterwards.
func SetAttribute(doc *pxf.Document, name string, value any) error {
	if doc == nil {
		return fmt.Errorf("attribute %q: nil document", name)
	}
	spec, ok := LookupAttribute(name)
	if !ok {
		return fmt.Errorf("unknown attribute %q", name)
	}
	pxfVal, err := encodeValue(value, spec.Kind)
	if err != nil {
		return fmt.Errorf("attribute %q: %w", name, err)
	}
	if spec.Validate != nil {
		if err := spec.Validate(value); err != nil {
			return fmt.Errorf("attribute %q: %w", name, err)
		}
	}
	blockName, fieldName, ok := splitName(name)
	if !ok {
		return fmt.Errorf("attribute %q: expected dotted block.field name", name)
	}
	block := findTopBlock(doc, blockName)
	if block == nil {
		block = &pxf.Block{Name: blockName}
		doc.Entries = append(doc.Entries, block)
	}
	if assign := findAssignment(block, fieldName); assign != nil {
		assign.Value = pxfVal
		return nil
	}
	block.Entries = append(block.Entries, &pxf.Assignment{
		Key:   fieldName,
		Value: pxfVal,
	})
	return nil
}

func splitName(name string) (block, field string, ok bool) {
	i := strings.IndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return "", "", false
	}
	return name[:i], name[i+1:], true
}

func findTopBlock(doc *pxf.Document, name string) *pxf.Block {
	for _, e := range doc.Entries {
		if b, ok := e.(*pxf.Block); ok && b.Name == name {
			return b
		}
	}
	return nil
}

func findAssignment(b *pxf.Block, key string) *pxf.Assignment {
	for _, e := range b.Entries {
		if a, ok := e.(*pxf.Assignment); ok && a.Key == key {
			return a
		}
	}
	return nil
}

func zeroFor(k AttributeKind) any {
	switch k {
	case KindString:
		return ""
	case KindStringList:
		return []string{}
	case KindBool:
		return false
	}
	return nil
}

func decodeValue(v pxf.Value, k AttributeKind) (any, error) {
	switch k {
	case KindString:
		sv, ok := v.(*pxf.StringVal)
		if !ok {
			return nil, fmt.Errorf("expected string value, got %T", v)
		}
		return sv.Value, nil
	case KindStringList:
		list, ok := v.(*pxf.ListVal)
		if !ok {
			return nil, fmt.Errorf("expected list value, got %T", v)
		}
		out := make([]string, 0, len(list.Elements))
		for i, elem := range list.Elements {
			sv, ok := elem.(*pxf.StringVal)
			if !ok {
				return nil, fmt.Errorf("list element %d: expected string, got %T", i, elem)
			}
			out = append(out, sv.Value)
		}
		return out, nil
	case KindBool:
		bv, ok := v.(*pxf.BoolVal)
		if !ok {
			return nil, fmt.Errorf("expected bool value, got %T", v)
		}
		return bv.Value, nil
	}
	return nil, fmt.Errorf("unsupported kind %v", k)
}

func encodeValue(v any, k AttributeKind) (pxf.Value, error) {
	switch k {
	case KindString:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("expected string, got %T", v)
		}
		return &pxf.StringVal{Value: s}, nil
	case KindStringList:
		ss, ok := v.([]string)
		if !ok {
			return nil, fmt.Errorf("expected []string, got %T", v)
		}
		elems := make([]pxf.Value, 0, len(ss))
		for _, s := range ss {
			elems = append(elems, &pxf.StringVal{Value: s})
		}
		return &pxf.ListVal{Elements: elems}, nil
	case KindBool:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("expected bool, got %T", v)
		}
		return &pxf.BoolVal{Value: b}, nil
	}
	return nil, fmt.Errorf("unsupported kind %v", k)
}
