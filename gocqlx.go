// Copyright (C) 2017 ScyllaDB
// Use of this source code is governed by a ALv2-style
// license that can be found in the LICENSE file.

package gocqlx

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/gocql/gocql"
	"github.com/scylladb/go-reflectx"
)

// reflect helpers

var unmarshallerInterface = reflect.TypeOf((*gocql.Unmarshaler)(nil)).Elem()
var udtUnmarshallerInterface = reflect.TypeOf((*gocql.Unmarshaler)(nil)).Elem()

func baseType(t reflect.Type, expected reflect.Kind) (reflect.Type, error) {
	t = reflectx.Deref(t)
	if t.Kind() != expected {
		return nil, fmt.Errorf("expected %s but got %s", expected, t.Kind())
	}
	return t, nil
}

// isScannable takes the reflect.Type and the actual dest value and returns
// whether or not it's Scannable. Something is scannable if:
//   * it is not a struct
//   * it implements gocql.Unmarshaler or gocql.UDTUnmarshaler
//   * it has no exported fields
func isScannable(t reflect.Type) bool {
	ptr := reflect.PtrTo(t)
	if ptr.Implements(unmarshallerInterface) || ptr.Implements(udtUnmarshallerInterface) {
		return true
	}
	if t.Kind() != reflect.Struct {
		return true
	}

	// it's not important that we use the right mapper for this particular object,
	// we're only concerned on how many exported fields this struct has
	m := DefaultMapper

	return len(m.TypeMap(t).Index) == 0
}

// fieldsByName fills a values interface with fields from the passed value based
// on the traversals in int. If ptrs is true, return addresses instead of values.
// We write this instead of using FieldsByName to save allocations and map lookups
// when iterating over many rows. Empty traversals will get an interface pointer.
// Because of the necessity of requesting ptrs or values, it's considered a bit too
// specialized for inclusion in reflectx itself.
func fieldsByTraversal(v reflect.Value, traversals [][]int, values []interface{}, ptrs bool) error {
	v = reflect.Indirect(v)
	if v.Kind() != reflect.Struct {
		return errors.New("argument not a struct")
	}

	for i, traversal := range traversals {
		if len(traversal) == 0 {
			continue
		}
		f := reflectx.FieldByIndexes(v, traversal)
		if ptrs {
			values[i] = f.Addr().Interface()
		} else {
			values[i] = f.Interface()
		}
	}
	return nil
}

func missingFields(transversals [][]int) (field int, err error) {
	for i, t := range transversals {
		if len(t) == 0 {
			return i, errors.New("missing field")
		}
	}
	return 0, nil
}
