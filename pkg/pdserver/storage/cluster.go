// Copyright 2016 DeepFabric, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package storage

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/deepfabric/elasticell/pkg/pb/metapb"
	"github.com/deepfabric/elasticell/pkg/util"
	"github.com/pkg/errors"
)

var (
	endID = uint64(math.MaxUint64)
)

// GetCurrentClusterMembers returns members in current etcd cluster
func (s *Store) GetCurrentClusterMembers() (*clientv3.MemberListResponse, error) {
	ctx, cancel := context.WithTimeout(s.client.Ctx(), DefaultRequestTimeout)
	members, err := s.client.MemberList(ctx)
	cancel()

	return members, errors.Wrap(err, "")
}

// GetClusterID returns current cluster id
// if cluster is not init, return 0
func (s *Store) GetClusterID() (uint64, error) {
	resp, err := s.get(pdClusterIDPath, clientv3.WithFirstCreate()...)

	if len(resp.Kvs) == 0 {
		return 0, err
	}

	key := string(resp.Kvs[0].Key)

	// If the key is "pdClusterIDPath", parse the cluster ID from it.
	if key == pdClusterIDPath {
		return util.BytesToUint64(resp.Kvs[0].Value)
	}

	// Parse the cluster ID from any other keys for compatibility.
	elems := strings.Split(key, "/")
	if len(elems) < 4 {
		return 0, errors.Errorf("invalid cluster key %v", key)
	}

	return strconv.ParseUint(elems[3], 10, 64)
}

// CreateFirstClusterID create the first cluster
// More than one pd instance do this operation at the first time,
// only one can succ,
// others will get the committed id.
func (s *Store) CreateFirstClusterID() (uint64, error) {
	ctx, cancel := context.WithTimeout(s.client.Ctx(), DefaultTimeout)
	defer cancel()

	// Generate a random cluster ID.
	ts := uint64(time.Now().Unix())
	clusterID := (ts << 32) + uint64(rand.Uint32())
	value := util.Uint64ToBytes(clusterID)

	resp, err := s.client.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(pdClusterIDPath), "=", 0)).
		Then(clientv3.OpPut(pdClusterIDPath, string(value))).
		Else(clientv3.OpGet(pdClusterIDPath)).
		Commit()

	if err != nil {
		return 0, errors.Wrap(err, "")
	}

	// Txn commits ok, return the generated cluster ID.
	if resp.Succeeded {
		return clusterID, nil
	}

	// Otherwise, parse the committed cluster ID.
	if len(resp.Responses) == 0 {
		return 0, errors.Errorf("txn returns empty response: %v", resp)
	}

	response := resp.Responses[0].GetResponseRange()
	if response == nil || len(response.Kvs) != 1 {
		return 0, errors.Errorf("txn returns invalid range response: %v", resp)
	}

	return util.BytesToUint64(response.Kvs[0].Value)
}

// SetClusterBootstrapped set cluster bootstrapped flag, only one can succ.
func (s *Store) SetClusterBootstrapped(clusterID uint64, cluster metapb.Cluster, store metapb.Store, cell metapb.Cell) (bool, error) {
	ctx, cancel := context.WithTimeout(s.client.Ctx(), DefaultTimeout)
	defer cancel()

	clusterBaseKey := s.getClusterMetaKey(clusterID)
	storeKey := s.getStoreMetaKey(clusterID, store.ID)
	cellKey := s.getCellMetaKey(clusterID, cell.ID)

	// build operations
	var ops []clientv3.Op

	meta, err := cluster.Marshal()
	if err != nil {
		return false, errors.Wrap(err, "")
	}
	ops = append(ops, clientv3.OpPut(clusterBaseKey, string(meta)))

	meta, err = store.Marshal()
	if err != nil {
		return false, errors.Wrap(err, "")
	}
	ops = append(ops, clientv3.OpPut(storeKey, string(meta)))

	meta, err = cell.Marshal()
	if err != nil {
		return false, errors.Wrap(err, "")
	}
	ops = append(ops, clientv3.OpPut(cellKey, string(meta)))

	// txn
	resp, err := s.client.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(clusterBaseKey), "=", 0)).
		Then(ops...).
		Commit()

	if err != nil {
		return false, errors.Wrap(err, "")
	}

	// already bootstrapped
	if !resp.Succeeded {
		return false, nil
	}

	return true, nil
}

// LoadClusterMeta returns cluster meta info
func (s *Store) LoadClusterMeta(clusterID uint64) (*metapb.Cluster, error) {
	key := s.getClusterMetaKey(clusterID)

	data, err := s.getValue(key)
	if err != nil {
		return nil, err
	}

	if nil == data {
		return nil, nil
	}

	v := &metapb.Cluster{}
	err = v.Unmarshal(data)
	return v, err
}

// LoadStoreMeta returns load error,
// do funcation will call on each loaded store meta info
func (s *Store) LoadStoreMeta(clusterID uint64, limit int64, do func(metapb.Store)) error {
	startID := uint64(0)
	endStore := s.getStoreMetaKey(clusterID, endID)
	withRange := clientv3.WithRange(endStore)
	withLimit := clientv3.WithLimit(limit)

	for {
		startKey := s.getStoreMetaKey(clusterID, startID)
		resp, err := s.get(startKey, withRange, withLimit)
		if err != nil {
			return err
		}

		for _, item := range resp.Kvs {
			v := &metapb.Store{}
			err := v.Unmarshal(item.Value)
			if err != nil {
				return errors.Wrap(err, "")
			}

			startID = v.ID + 1
			do(*v)
		}

		// read complete
		if len(resp.Kvs) < int(limit) {
			break
		}
	}

	return nil
}

// LoadCellMeta returns load error,
// do funcation will call on each loaded cell meta info
func (s *Store) LoadCellMeta(clusterID uint64, limit int64, do func(metapb.Cell)) error {
	startID := uint64(0)
	endCellKey := s.getCellMetaKey(clusterID, endID)
	withRange := clientv3.WithRange(endCellKey)
	withLimit := clientv3.WithLimit(limit)

	for {
		startKey := s.getCellMetaKey(clusterID, startID)
		resp, err := s.get(startKey, withRange, withLimit)
		if err != nil {
			return err
		}

		for _, item := range resp.Kvs {
			v := &metapb.Cell{}
			err := v.Unmarshal(item.Value)

			if err != nil {
				return errors.Wrap(err, "")
			}

			startID = v.ID + 1
			do(*v)
		}

		// read complete
		if len(resp.Kvs) < int(limit) {
			break
		}
	}

	return nil
}

// SetStoreMeta returns nil if store is add or update succ
func (s *Store) SetStoreMeta(clusterID uint64, store metapb.Store) error {
	key := s.getStoreMetaKey(clusterID, store.ID)
	meta, err := store.Marshal()
	if err != nil {
		return errors.Wrap(err, "")
	}

	return s.save(key, string(meta))
}

// SetCellMeta returns nil if cell is add or update succ
func (s *Store) SetCellMeta(clusterID uint64, cell metapb.Cell) error {
	cellKey := s.getCellMetaKey(clusterID, cell.ID)
	meta, err := cell.Marshal()
	if err != nil {
		return errors.Wrap(err, "")
	}

	return s.save(cellKey, string(meta))
}

func (s *Store) getClusterMetaKey(clusterID uint64) string {
	return fmt.Sprintf("%s/%d", pdClusterRootPath, clusterID)
}

func (s *Store) getStoreMetaKey(clusterID, storeID uint64) string {
	baseKey := s.getClusterMetaKey(clusterID)
	return fmt.Sprintf("%s/stores/%020d", baseKey, storeID)
}

func (s *Store) getCellMetaKey(clusterID, cellID uint64) string {
	baseKey := s.getClusterMetaKey(clusterID)
	return fmt.Sprintf("%s/cells/%020d", baseKey, cellID)
}
