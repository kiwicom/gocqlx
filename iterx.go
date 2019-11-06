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

// Get is a convenience function for creating iterator and calling Get.
//
// DEPRECATED use Queryx.Get or Queryx.GetRelease.
func Get(dest interface{}, q *gocql.Query) error {
	return Iter(q).Get(dest)
}

// Select is a convenience function for creating iterator and calling Select.
//
// DEPRECATED use Queryx.Select or Queryx.SelectRelease.
func Select(dest interface{}, q *gocql.Query) error {
	return Iter(q).Select(dest)
}

// Iterx is a wrapper around gocql.Iter which adds struct scanning capabilities.
type Iterx struct {
	*gocql.Iter
	err error

	unsafe      bool
	forceStruct bool

	Mapper *reflectx.Mapper
	// these fields cache memory use for a rows during iteration w/ structScan
	started bool
	fields  [][]int
	values  []interface{}
}

// Iter creates a new Iterx from gocql.Query using a default mapper.
func Iter(q *gocql.Query) *Iterx {
	return &Iterx{
		Iter:   q.Iter(),
		Mapper: DefaultMapper,
	}
}

// Unsafe forces the iterator to ignore missing fields. By default when scanning
// a struct if result row has a column that cannot be mapped to any destination
// field an error is reported. With unsafe such columns are ignored.
func (iter *Iterx) Unsafe() *Iterx {
	iter.unsafe = true
	return iter
}

// Struct forces the iterator to treat a single-argument struct as non-scannable.
// This is is useful if you need to scan a row into a struct that also implements gocql.Unmarshaler
// or gocql.UDTUnmarshaler.
func (iter *Iterx) Struct() *Iterx {
	iter.forceStruct = true
	return iter
}

// Get scans first row into a destination and closes the iterator.
//
// If the destination type is scannable (non-struct, gocql.Unmarshaler, gocql.Marshaler), the row must have only
// one column which can scan into that type.
// If the destination type is non-scannable struct pointer or Struct() was used on the iterator, then
// StructScan will be used.
//
// If no rows were selected, ErrNotFound is returned.
func (iter *Iterx) Get(dest interface{}) error {
	if iter.forceStruct {
		iter.StructScan(dest)
	} else {
		iter.scanAny(dest)
	}
	iter.Close()

	return iter.checkErrAndNotFound()
}

func (iter *Iterx) scanAny(dest interface{}) bool {
	value := reflect.ValueOf(dest)
	if value.Kind() != reflect.Ptr {
		iter.err = errors.New("must pass a pointer, not a value, to StructScan destination")
		return false
	}
	if value.IsNil() {
		iter.err = errors.New("nil pointer passed to StructScan destination")
		return false
	}

	base := reflectx.Deref(value.Type())
	scannable := isScannable(base)

	if scannable && len(iter.Columns()) > 1 {
		iter.err = fmt.Errorf("scannable dest type %s with >1 columns (%d) in result", base.Kind(), len(iter.Columns()))
		return false
	}

	if scannable {
		return iter.Scan(dest)
	}

	return iter.StructScan(dest)
}

// Select scans all rows into a destination.
//
// If the destination type is slice of scannable types (non-struct, gocql.Unmarshaler, gocql.Marshaler), each row must
// have only one column which can scan into that type.
// If the destination type is slice of non-scannable struct pointers or Struct() was used on the iterator, then
// StructScan will be used on each row.
//
// If no rows were selected, ErrNotFound is NOT returned.
func (iter *Iterx) Select(dest interface{}) error {
	iter.scanAll(dest)
	iter.Close()

	return iter.err
}

func (iter *Iterx) scanAll(dest interface{}) bool {
	value := reflect.ValueOf(dest)

	// json.Unmarshal returns errors for these
	if value.Kind() != reflect.Ptr {
		iter.err = errors.New("must pass a pointer, not a value, to StructScan destination")
		return false
	}
	if value.IsNil() {
		iter.err = errors.New("nil pointer passed to StructScan destination")
		return false
	}

	slice, err := baseType(value.Type(), reflect.Slice)
	if err != nil {
		iter.err = err
		return false
	}

	isPtr := slice.Elem().Kind() == reflect.Ptr
	base := reflectx.Deref(slice.Elem())
	scannable := isScannable(base)

	if iter.forceStruct {
		if base.Kind() != reflect.Struct {
			iter.err = fmt.Errorf("non-struct dest type %s with StructSelect", base.Kind())
			return false
		}
	} else {
		// if it's a base type make sure it only has 1 column;  if not return an error
		if scannable && len(iter.Columns()) > 1 {
			iter.err = fmt.Errorf("non-struct dest type %s with >1 columns (%d)", base.Kind(), len(iter.Columns()))
			return false
		}
	}

	var (
		alloc bool
		v     reflect.Value
		vp    reflect.Value
		ok    bool
	)
	for {
		// create a new struct type (which returns PtrTo) and indirect it
		vp = reflect.New(base)

		// scan into the struct field pointers
		if iter.forceStruct || !scannable {
			ok = iter.StructScan(vp.Interface())
		} else {
			ok = iter.Scan(vp.Interface())
		}
		if !ok {
			break
		}

		// allocate memory for the page data
		if !alloc {
			v = reflect.MakeSlice(slice, 0, iter.NumRows())
			alloc = true
		}

		if isPtr {
			v = reflect.Append(v, vp)
		} else {
			v = reflect.Append(v, reflect.Indirect(vp))
		}
	}

	// update dest if allocated slice
	if alloc {
		reflect.Indirect(value).Set(v)
	}

	return true
}

// StructScan is like gocql.Iter.Scan, but scans a single row into a single
// struct. Use this and iterate manually when the memory load of Select() might
// be prohibitive. StructScan caches the reflect work of matching up column
// positions to fields to avoid that overhead per scan, which means it is not
// safe to run StructScan on the same Iterx instance with different struct
// types.
func (iter *Iterx) StructScan(dest interface{}) bool {
	v := reflect.ValueOf(dest)
	if v.Kind() != reflect.Ptr {
		iter.err = errors.New("must pass a pointer, not a value, to StructScan destination")
		return false
	}

	if !iter.started {
		columns := columnNames(iter.Iter.Columns())
		m := iter.Mapper

		iter.fields = m.TraversalsByName(v.Type(), columns)
		// if we are not unsafe and are missing fields, return an error
		if !iter.unsafe {
			if f, err := missingFields(iter.fields); err != nil {
				iter.err = fmt.Errorf("missing destination name %q in %T", columns[f], dest)
				return false
			}
		}
		iter.values = make([]interface{}, len(columns))
		iter.started = true
	}

	err := fieldsByTraversal(v, iter.fields, iter.values, true)
	if err != nil {
		iter.err = err
		return false
	}
	// scan into the struct field pointers and append to our results
	return iter.Iter.Scan(iter.values...)
}

func columnNames(ci []gocql.ColumnInfo) []string {
	r := make([]string, len(ci))
	for i, column := range ci {
		r[i] = column.Name
	}
	return r
}

// Close closes the iterator and returns any errors that happened during
// the query or the iteration.
func (iter *Iterx) Close() error {
	err := iter.Iter.Close()
	if iter.err == nil {
		iter.err = err
	}
	return iter.err
}

// checkErrAndNotFound handle error and NotFound in one method.
func (iter *Iterx) checkErrAndNotFound() error {
	if iter.err != nil {
		return iter.err
	} else if iter.Iter.NumRows() == 0 {
		return gocql.ErrNotFound
	}
	return nil
}
