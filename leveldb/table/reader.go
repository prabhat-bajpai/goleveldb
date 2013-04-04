// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package table allows read and write sorted key/value.
package table

import (
	"runtime"
	"strings"

	"github.com/syndtr/goleveldb/leveldb/block"
	"github.com/syndtr/goleveldb/leveldb/cache"
	"github.com/syndtr/goleveldb/leveldb/comparer"
	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/storage"
)

// Reader represent a table reader.
type Reader struct {
	r storage.Reader
	o opt.OptionsGetter

	indexBlock  *block.Reader
	filterBlock *block.FilterReader

	dataEnd uint64
	cache   cache.Namespace
}

// NewReader create new initialized table reader.
func NewReader(r storage.Reader, size uint64, o opt.OptionsGetter, cache cache.Namespace) (p *Reader, err error) {
	mb, ib, err := readFooter(r, size)
	if err != nil {
		return
	}

	t := &Reader{r: r, o: o, dataEnd: mb.offset, cache: cache}

	// index block
	buf, err := ib.readAll(r, true)
	if err != nil {
		return
	}
	t.indexBlock, err = block.NewReader(buf, o.GetComparer())
	if err != nil {
		return
	}

	// we will ignore any errors at meta/filter block
	// since it is not essential for operation

	// meta block
	buf, err1 := mb.readAll(r, true)
	if err1 != nil {
		return
	}
	meta, err1 := block.NewReader(buf, comparer.BytesComparer{})
	if err1 != nil {
		return
	}

	// filter block
	iter := meta.NewIterator()
	for iter.Next() {
		key := string(iter.Key())
		if !strings.HasPrefix(key, "filter.") {
			continue
		}
		if filter := o.GetAltFilter(key[7:]); filter != nil {
			fb := new(bInfo)
			_, err1 = fb.decodeFrom(iter.Value())
			if err1 != nil {
				continue
			}

			// now the data block end before filter block start offset
			// instead of meta block start offset
			t.dataEnd = fb.offset

			buf, err1 = fb.readAll(r, true)
			if err1 != nil {
				continue
			}
			t.filterBlock, err1 = block.NewFilterReader(buf, filter)
			if err1 != nil {
				continue
			}
			break
		}
	}

	return t, nil
}

// NewIterator create new iterator over the table.
func (t *Reader) NewIterator(ro opt.ReadOptionsGetter) iterator.Iterator {
	index_iter := &indexIter{t: t, ro: ro}
	t.indexBlock.InitIterator(&index_iter.Iterator)
	return iterator.NewIndexedIterator(index_iter)
}

// Get lookup for given key on the table. Get returns errors.ErrNotFound if
// given key did not exist.
func (t *Reader) Get(key []byte, ro opt.ReadOptionsGetter) (rkey, rvalue []byte, err error) {
	// create an iterator of index block
	index_iter := t.indexBlock.NewIterator()
	if !index_iter.Seek(key) {
		err = index_iter.Error()
		if err == nil {
			err = errors.ErrNotFound
		}
		return
	}

	// decode data block info
	bi := new(bInfo)
	_, err = bi.decodeFrom(index_iter.Value())
	if err != nil {
		return
	}

	// get the data block
	if t.filterBlock == nil || t.filterBlock.KeyMayMatch(uint(bi.offset), key) {
		var it iterator.Iterator
		var cache cache.Object
		it, cache, err = t.getDataIter(bi, ro)
		if err != nil {
			return
		}
		if cache != nil {
			defer cache.Release()
		}

		// seek to key
		if !it.Seek(key) {
			err = it.Error()
			if err == nil {
				err = errors.ErrNotFound
			}
			return
		}
		rkey, rvalue = it.Key(), it.Value()
	} else {
		err = errors.ErrNotFound
	}
	return
}

// ApproximateOffsetOf approximate the offset of given key in bytes.
func (t *Reader) ApproximateOffsetOf(key []byte) uint64 {
	index_iter := t.indexBlock.NewIterator()
	if index_iter.Seek(key) {
		bi := new(bInfo)
		_, err := bi.decodeFrom(index_iter.Value())
		if err == nil {
			return bi.offset
		}
	}
	// block info is corrupted or key is past the last key in the file.
	// Approximate the offset by returning offset of the end of data
	// block (which is right near the end of the file).
	return t.dataEnd
}

func (t *Reader) getBlock(bi *bInfo, ro opt.ReadOptionsGetter) (b *block.Reader, err error) {
	buf, err := bi.readAll(t.r, ro.HasFlag(opt.RFVerifyChecksums))
	if err != nil {
		return
	}
	b, err = block.NewReader(buf, t.o.GetComparer())
	return
}

func (t *Reader) getDataIter(bi *bInfo, ro opt.ReadOptionsGetter) (it *block.Iterator, cache cache.Object, err error) {
	var b *block.Reader

	if t.cache != nil {
		var ok bool
		cache, ok = t.cache.Get(bi.offset, func() (ok bool, value interface{}, charge int, fin func()) {
			if ro.HasFlag(opt.RFDontFillCache) {
				return
			}
			b, err = t.getBlock(bi, ro)
			if err == nil {
				ok = true
				value = b
				charge = int(bi.size)
			}
			return
		})

		if err != nil {
			return
		}

		if !ok {
			b, err = t.getBlock(bi, ro)
			if err != nil {
				return
			}
		} else if b == nil {
			b = cache.Value().(*block.Reader)
		}
	} else {
		b, err = t.getBlock(bi, ro)
		if err != nil {
			return
		}
	}

	it = b.NewIterator()
	return
}

type indexIter struct {
	block.Iterator

	t  *Reader
	ro opt.ReadOptionsGetter
}

func (i *indexIter) Get() (it iterator.Iterator, err error) {
	bi := new(bInfo)
	_, err = bi.decodeFrom(i.Value())
	if err != nil {
		return
	}

	x, cache, err := i.t.getDataIter(bi, i.ro)
	if err != nil {
		return
	}
	if cache != nil {
		runtime.SetFinalizer(x, func(x *block.Iterator) {
			cache.Release()
		})
	}
	return x, nil
}
