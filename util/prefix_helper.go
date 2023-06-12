// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Copyright 2014 The ql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSES/QL-LICENSE file.

package util

import (
	"bytes"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/kv"
)

// ScanMetaWithPrefix scans metadata with the prefix.
func ScanMetaWithPrefix(retriever kv.Retriever, prefix kv.Key, filter func(kv.Key, []byte) bool) error {
	iter, err := retriever.Iter(prefix, prefix.PrefixNext())
	if err != nil {
		return errors.Trace(err)
	}
	defer iter.Close()

	for {
		if !(iter.Valid() && iter.Key().HasPrefix(prefix)) {
			break
		}
		if !filter(iter.Key(), iter.Value()) {
			break
		}
		err = iter.Next()
		if err != nil {
			return errors.Trace(err)
		}
	}

	return nil
}

// DelKeyWithPrefix deletes keys with prefix.
func DelKeyWithPrefix(rm kv.RetrieverMutator, prefix kv.Key) error {
	var keys []kv.Key
	iter, err := rm.Iter(prefix, prefix.PrefixNext())
	if err != nil {
		return errors.Trace(err)
	}

	defer iter.Close()
	for {
		if !(iter.Valid() && iter.Key().HasPrefix(prefix)) {
			break
		}
		keys = append(keys, iter.Key().Clone())
		err = iter.Next()
		if err != nil {
			return errors.Trace(err)
		}
	}

	for _, key := range keys {
		err := rm.Delete(key)
		if err != nil {
			return errors.Trace(err)
		}
	}

	return nil
}

// RowKeyPrefixFilter returns a function which checks whether currentKey has decoded rowKeyPrefix as prefix.
func RowKeyPrefixFilter(rowKeyPrefix kv.Key) kv.FnKeyCmp {
	return func(currentKey kv.Key) bool {
		// Next until key without prefix of this record.
		return !bytes.HasPrefix(currentKey, rowKeyPrefix)
	}
}
