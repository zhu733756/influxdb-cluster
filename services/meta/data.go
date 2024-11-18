package meta

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gogo/protobuf/proto"
	"github.com/influxdata/influxdb"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/query"
	internal "github.com/influxdata/influxdb/services/meta/internal"
	"github.com/influxdata/influxql"
)

//go:generate protoc --gogo_out=. internal/meta.proto

const (
	// DefaultRetentionPolicyReplicaN is the default value of RetentionPolicyInfo.ReplicaN.
	DefaultRetentionPolicyReplicaN = 1

	// MaxAutoCreatedRetentionPolicyReplicaN is the maximum replication factor that will
	// be set for auto-created retention policies.
	MaxAutoCreatedRetentionPolicyReplicaN = 3

	// DefaultRetentionPolicyDuration is the default value of RetentionPolicyInfo.Duration.
	DefaultRetentionPolicyDuration = time.Duration(0)

	// DefaultRetentionPolicyName is the default name for auto generated retention policies.
	DefaultRetentionPolicyName = "autogen"

	// MinRetentionPolicyDuration represents the minimum duration for a policy.
	MinRetentionPolicyDuration = time.Hour

	// MaxNameLen is the maximum length of a database or retention policy name.
	// InfluxDB uses the name for the directory name on disk.
	MaxNameLen = 255
)

// Data represents the top level collection of all metadata.
type Data struct {
	Term      uint64 // associated raft term
	Index     uint64 // associated raft index
	ClusterID uint64
	MetaNodes []NodeInfo
	DataNodes []NodeInfo
	Databases []DatabaseInfo
	Users     []UserInfo

	// adminUserExists provides a constant time mechanism for determining
	// if there is at least one admin user.
	adminUserExists bool

	MaxNodeID       uint64
	MaxShardGroupID uint64
	MaxShardID      uint64
}

// DataNode returns a node by id.
func (data *Data) DataNode(id uint64) *NodeInfo {
	for i := range data.DataNodes {
		if data.DataNodes[i].ID == id {
			return &data.DataNodes[i]
		}
	}
	return nil
}

// CreateDataNode adds a node to the metadata.
func (data *Data) CreateDataNode(addr, tcpAddr string) error {
	// Ensure a node with the same host doesn't already exist.
	for _, n := range data.DataNodes {
		if n.TCPAddr == tcpAddr {
			return ErrNodeExists
		}
	}

	// If an existing meta node exists with the same TCPHost address,
	// then these nodes are actually the same so re-use the existing ID
	var existingID uint64
	for _, n := range data.MetaNodes {
		if n.TCPAddr == tcpAddr {
			existingID = n.ID
			break
		}
	}

	// We didn't find an existing node, so assign it a new node ID
	if existingID == 0 {
		data.MaxNodeID++
		existingID = data.MaxNodeID
	}

	// Append new node.
	data.DataNodes = append(data.DataNodes, NodeInfo{
		ID:      existingID,
		Addr:    addr,
		TCPAddr: tcpAddr,
	})
	sort.Sort(NodeInfos(data.DataNodes))

	return nil
}

// setDataNode adds a data node with a pre-specified nodeID.
// this should only be used when the cluster is upgrading from 0.9 to 0.10
func (data *Data) setDataNode(nodeID uint64, addr, tcpAddr string) error {
	// Ensure a node with the same host doesn't already exist.
	for _, n := range data.DataNodes {
		if n.Addr == addr {
			return ErrNodeExists
		}
	}

	// Append new node.
	data.DataNodes = append(data.DataNodes, NodeInfo{
		ID:      nodeID,
		Addr:    addr,
		TCPAddr: tcpAddr,
	})

	return nil
}

// DeleteDataNode removes a node from the Meta store.
//
// If necessary, DeleteDataNode reassigns ownership of any shards that
// would otherwise become orphaned by the removal of the node from the
// cluster.
func (data *Data) DeleteDataNode(id uint64) error {
	var nodes []NodeInfo

	// Remove the data node from the store's list.
	for _, n := range data.DataNodes {
		if n.ID != id {
			nodes = append(nodes, n)
		}
	}

	if len(nodes) == len(data.DataNodes) {
		return ErrNodeNotFound
	}
	data.DataNodes = nodes

	// Remove node id from all shard infos
	for di, d := range data.Databases {
		for ri, rp := range d.RetentionPolicies {
			for sgi, sg := range rp.ShardGroups {
				var (
					nodeOwnerFreqs = make(map[int]int)
					orphanedShards []ShardInfo
				)
				// Look through all shards in the shard group and
				// determine (1) if a shard no longer has any owners
				// (orphaned); (2) if all shards in the shard group
				// are orphaned; and (3) the number of shards in this
				// group owned by each data node in the cluster.
				for si, s := range sg.Shards {
					// Track of how many shards in the group are
					// owned by each data node in the cluster.
					var nodeIdx = -1
					for i, owner := range s.Owners {
						if owner.NodeID == id {
							nodeIdx = i
						}
						nodeOwnerFreqs[int(owner.NodeID)]++
					}

					if nodeIdx > -1 {
						// Data node owns shard, so relinquish ownership
						// and set new owners on the shard.
						s.Owners = append(s.Owners[:nodeIdx], s.Owners[nodeIdx+1:]...)
						data.Databases[di].RetentionPolicies[ri].ShardGroups[sgi].Shards[si].Owners = s.Owners
					}

					// Shard no longer owned. Will need reassigning
					// an owner.
					if len(s.Owners) == 0 {
						orphanedShards = append(orphanedShards, s)
					}
				}

				// Mark the shard group as deleted if it has no shards,
				// or all of its shards are orphaned.
				if len(sg.Shards) == 0 || len(orphanedShards) == len(sg.Shards) {
					data.Databases[di].RetentionPolicies[ri].ShardGroups[sgi].DeletedAt = time.Now().UTC()
					continue
				}

				// Reassign any orphaned shards. Delete the node we're
				// dropping from the list of potential new owners.
				delete(nodeOwnerFreqs, int(id))

				for _, orphan := range orphanedShards {
					newOwnerID, err := newShardOwner(orphan, nodeOwnerFreqs)
					if err != nil {
						return err
					}

					for si, s := range sg.Shards {
						if s.ID == orphan.ID {
							sg.Shards[si].Owners = append(sg.Shards[si].Owners, ShardOwner{NodeID: newOwnerID})
							data.Databases[di].RetentionPolicies[ri].ShardGroups[sgi].Shards = sg.Shards
							break
						}
					}

				}
			}
		}
	}
	return nil
}

// newShardOwner sets the owner of the provided shard to the data node
// that currently owns the fewest number of shards. If multiple nodes
// own the same (fewest) number of shards, then one of those nodes
// becomes the new shard owner.
func newShardOwner(s ShardInfo, ownerFreqs map[int]int) (uint64, error) {
	var (
		minId   = -1
		minFreq int
	)

	for id, freq := range ownerFreqs {
		if minId == -1 || freq < minFreq {
			minId, minFreq = int(id), freq
		}
	}

	if minId < 0 {
		return 0, fmt.Errorf("cannot reassign shard %d due to lack of data nodes", s.ID)
	}

	// Update the shard owner frequencies and set the new owner on the
	// shard.
	ownerFreqs[minId]++
	return uint64(minId), nil
}

// MetaNode returns a node by id.
func (data *Data) MetaNode(id uint64) *NodeInfo {
	for i := range data.MetaNodes {
		if data.MetaNodes[i].ID == id {
			return &data.MetaNodes[i]
		}
	}
	return nil
}

// CreateMetaNode will add a new meta node to the metastore
func (data *Data) CreateMetaNode(httpAddr, tcpAddr string) error {
	// Ensure a node with the same host doesn't already exist.
	for _, n := range data.MetaNodes {
		if n.Addr == httpAddr {
			return ErrNodeExists
		}
	}

	// If an existing data node exists with the same TCPHost address,
	// then these nodes are actually the same so re-use the existing ID
	var existingID uint64
	for _, n := range data.DataNodes {
		if n.TCPAddr == tcpAddr {
			existingID = n.ID
			break
		}
	}

	// We didn't find and existing data node ID, so assign a new ID
	// to this meta node.
	if existingID == 0 {
		data.MaxNodeID++
		existingID = data.MaxNodeID
	}

	// Append new node.
	data.MetaNodes = append(data.MetaNodes, NodeInfo{
		ID:      existingID,
		Addr:    httpAddr,
		TCPAddr: tcpAddr,
	})

	sort.Sort(NodeInfos(data.MetaNodes))
	return nil
}

// SetMetaNode will update the information for the single meta
// node or create a new metanode. If there are more than 1 meta
// nodes already, an error will be returned
func (data *Data) SetMetaNode(httpAddr, tcpAddr string) error {
	if len(data.MetaNodes) > 1 {
		return fmt.Errorf("can't set meta node when there are more than 1 in the metastore")
	}

	if len(data.MetaNodes) == 0 {
		return data.CreateMetaNode(httpAddr, tcpAddr)
	}

	data.MetaNodes[0].Addr = httpAddr
	data.MetaNodes[0].TCPAddr = tcpAddr

	return nil
}

// DeleteMetaNode will remove the meta node from the store
func (data *Data) DeleteMetaNode(id uint64) error {
	// Node has to be larger than 0 to be real
	if id == 0 {
		return ErrNodeIDRequired
	}

	var nodes []NodeInfo
	for _, n := range data.MetaNodes {
		if n.ID == id {
			continue
		}
		nodes = append(nodes, n)
	}

	if len(nodes) == len(data.MetaNodes) {
		return ErrNodeNotFound
	}

	data.MetaNodes = nodes
	return nil
}

// Database returns a DatabaseInfo by the database name.
func (data *Data) Database(name string) *DatabaseInfo {
	for i := range data.Databases {
		if data.Databases[i].Name == name {
			return &data.Databases[i]
		}
	}
	return nil
}

// CloneDatabases returns a copy of the DatabaseInfo.
func (data *Data) CloneDatabases() []DatabaseInfo {
	if data.Databases == nil {
		return nil
	}
	dbs := make([]DatabaseInfo, len(data.Databases))
	for i := range data.Databases {
		dbs[i] = data.Databases[i].clone()
	}
	return dbs
}

// CreateDatabase creates a new database.
// It returns an error if name is blank or if a database with the same name already exists.
func (data *Data) CreateDatabase(name string) error {
	if name == "" {
		return ErrDatabaseNameRequired
	} else if len(name) > MaxNameLen {
		return ErrNameTooLong
	} else if data.Database(name) != nil {
		return nil
	}

	// Append new node.
	data.Databases = append(data.Databases, DatabaseInfo{Name: name})

	return nil
}

// DropDatabase removes a database by name. It does not return an error
// if the database cannot be found.
func (data *Data) DropDatabase(name string) error {
	for i := range data.Databases {
		if data.Databases[i].Name == name {
			data.Databases = append(data.Databases[:i], data.Databases[i+1:]...)

			// Remove all user privileges associated with this database.
			for i := range data.Users {
				delete(data.Users[i].Privileges, name)
			}
			break
		}
	}
	return nil
}

// RetentionPolicy returns a retention policy for a database by name.
func (data *Data) RetentionPolicy(database, name string) (*RetentionPolicyInfo, error) {
	di := data.Database(database)
	if di == nil {
		return nil, influxdb.ErrDatabaseNotFound(database)
	}

	for i := range di.RetentionPolicies {
		if di.RetentionPolicies[i].Name == name {
			return &di.RetentionPolicies[i], nil
		}
	}
	return nil, nil
}

// CreateRetentionPolicy creates a new retention policy on a database.
// It returns an error if name is blank or if the database does not exist.
func (data *Data) CreateRetentionPolicy(database string, rpi *RetentionPolicyInfo, makeDefault bool) error {
	// Validate retention policy.
	if rpi == nil {
		return ErrRetentionPolicyRequired
	} else if rpi.Name == "" {
		return ErrRetentionPolicyNameRequired
	} else if len(rpi.Name) > MaxNameLen {
		return ErrNameTooLong
	} else if rpi.ReplicaN < 1 {
		return ErrReplicationFactorTooLow
	}

	// Normalise ShardDuration before comparing to any existing
	// retention policies. The client is supposed to do this, but
	// do it again to verify input.
	rpi.ShardGroupDuration = normalisedShardDuration(rpi.ShardGroupDuration, rpi.Duration)

	if rpi.Duration > 0 && rpi.Duration < rpi.ShardGroupDuration {
		return ErrIncompatibleDurations
	}

	// Find database.
	di := data.Database(database)
	if di == nil {
		return influxdb.ErrDatabaseNotFound(database)
	} else if rp := di.RetentionPolicy(rpi.Name); rp != nil {
		// RP with that name already exists. Make sure they're the same.
		if rp.ReplicaN != rpi.ReplicaN || rp.Duration != rpi.Duration || rp.ShardGroupDuration != rpi.ShardGroupDuration {
			return ErrRetentionPolicyExists
		}
		// if they want to make it default, and it's not the default, it's not an identical command so it's an error
		if makeDefault && di.DefaultRetentionPolicy != rpi.Name {
			return ErrRetentionPolicyConflict
		}
		return nil
	}

	// Append copy of new policy.
	di.RetentionPolicies = append(di.RetentionPolicies, *rpi)

	// Set the default if needed
	if makeDefault {
		di.DefaultRetentionPolicy = rpi.Name
	}

	return nil
}

// DropRetentionPolicy removes a retention policy from a database by name.
func (data *Data) DropRetentionPolicy(database, name string) error {
	// Find database.
	di := data.Database(database)
	if di == nil {
		// no database? no problem
		return nil
	}

	// Remove from list.
	for i := range di.RetentionPolicies {
		if di.RetentionPolicies[i].Name == name {
			di.RetentionPolicies = append(di.RetentionPolicies[:i], di.RetentionPolicies[i+1:]...)
			break
		}
	}

	return nil
}

// RetentionPolicyUpdate represents retention policy fields to be updated.
type RetentionPolicyUpdate struct {
	Name               *string
	Duration           *time.Duration
	ReplicaN           *int
	ShardGroupDuration *time.Duration
}

// SetName sets the RetentionPolicyUpdate.Name.
func (rpu *RetentionPolicyUpdate) SetName(v string) { rpu.Name = &v }

// SetDuration sets the RetentionPolicyUpdate.Duration.
func (rpu *RetentionPolicyUpdate) SetDuration(v time.Duration) { rpu.Duration = &v }

// SetReplicaN sets the RetentionPolicyUpdate.ReplicaN.
func (rpu *RetentionPolicyUpdate) SetReplicaN(v int) { rpu.ReplicaN = &v }

// SetShardGroupDuration sets the RetentionPolicyUpdate.ShardGroupDuration.
func (rpu *RetentionPolicyUpdate) SetShardGroupDuration(v time.Duration) { rpu.ShardGroupDuration = &v }

// UpdateRetentionPolicy updates an existing retention policy.
func (data *Data) UpdateRetentionPolicy(database, name string, rpu *RetentionPolicyUpdate, makeDefault bool) error {
	// Find database.
	di := data.Database(database)
	if di == nil {
		return influxdb.ErrDatabaseNotFound(database)
	}

	// Find policy.
	rpi := di.RetentionPolicy(name)
	if rpi == nil {
		return influxdb.ErrRetentionPolicyNotFound(name)
	}

	// Ensure new policy doesn't match an existing policy.
	if rpu.Name != nil && *rpu.Name != name && di.RetentionPolicy(*rpu.Name) != nil {
		return ErrRetentionPolicyNameExists
	}

	// Enforce duration of at least MinRetentionPolicyDuration
	if rpu.Duration != nil && *rpu.Duration < MinRetentionPolicyDuration && *rpu.Duration != 0 {
		return ErrRetentionPolicyDurationTooLow
	}

	// Enforce duration is at least the shard duration
	if (rpu.Duration != nil && *rpu.Duration > 0 &&
		((rpu.ShardGroupDuration != nil && *rpu.Duration < *rpu.ShardGroupDuration) ||
			(rpu.ShardGroupDuration == nil && *rpu.Duration < rpi.ShardGroupDuration))) ||
		(rpu.Duration == nil && rpi.Duration > 0 &&
			rpu.ShardGroupDuration != nil && rpi.Duration < *rpu.ShardGroupDuration) {
		return ErrIncompatibleDurations
	}

	// Update fields.
	if rpu.Name != nil {
		rpi.Name = *rpu.Name
	}
	if rpu.Duration != nil {
		rpi.Duration = *rpu.Duration
	}
	if rpu.ReplicaN != nil {
		rpi.ReplicaN = *rpu.ReplicaN
	}
	if rpu.ShardGroupDuration != nil {
		rpi.ShardGroupDuration = normalisedShardDuration(*rpu.ShardGroupDuration, rpi.Duration)
	}

	if di.DefaultRetentionPolicy != rpi.Name && makeDefault {
		di.DefaultRetentionPolicy = rpi.Name
	}

	return nil
}

// DropShard removes a shard by ID.
//
// DropShard won't return an error if the shard can't be found, which
// allows the command to be re-run in the case that the meta store
// succeeds but a data node fails.
func (data *Data) DropShard(id uint64) {
	found := -1
	for dbidx, dbi := range data.Databases {
		for rpidx, rpi := range dbi.RetentionPolicies {
			for sgidx, sg := range rpi.ShardGroups {
				for sidx, s := range sg.Shards {
					if s.ID == id {
						found = sidx
						break
					}
				}

				if found > -1 {
					shards := sg.Shards
					data.Databases[dbidx].RetentionPolicies[rpidx].ShardGroups[sgidx].Shards = append(shards[:found], shards[found+1:]...)

					if len(shards) == 1 {
						// We just deleted the last shard in the shard group.
						data.Databases[dbidx].RetentionPolicies[rpidx].ShardGroups[sgidx].DeletedAt = time.Now()
					}
					return
				}
			}
		}
	}
}

// CopyShardOwner copies a shard owner by ID and NodeID.
func (data *Data) CopyShardOwner(id, nodeID uint64) {
	found := -1
	for dbidx, dbi := range data.Databases {
		for rpidx, rpi := range dbi.RetentionPolicies {
			for sgidx, sg := range rpi.ShardGroups {
				for sidx, s := range sg.Shards {
					if s.ID == id {
						found = sidx
						break
					}
				}

				if found > -1 {
					nodeIdx := -1
					s := sg.Shards[found]
					for i, owner := range s.Owners {
						if owner.NodeID == nodeID {
							return
						}
						if owner.NodeID > nodeID && nodeIdx == -1 {
							nodeIdx = i
						}
					}

					if nodeIdx > -1 {
						s.Owners = append(s.Owners[:nodeIdx+1], s.Owners[nodeIdx:]...)
						s.Owners[nodeIdx] = ShardOwner{NodeID: nodeID}

					} else {
						s.Owners = append(s.Owners, ShardOwner{NodeID: nodeID})
					}

					data.Databases[dbidx].RetentionPolicies[rpidx].ShardGroups[sgidx].Shards[found].Owners = s.Owners
					return
				}
			}
		}
	}
}

// RemoveShardOwner removes a shard owner by ID and NodeID.
func (data *Data) RemoveShardOwner(id, nodeID uint64) {
	found := -1
	for dbidx, dbi := range data.Databases {
		for rpidx, rpi := range dbi.RetentionPolicies {
			for sgidx, sg := range rpi.ShardGroups {
				for sidx, s := range sg.Shards {
					if s.ID == id {
						found = sidx
						break
					}
				}

				if found > -1 {
					nodeIdx := -1
					s := sg.Shards[found]
					for i, owner := range s.Owners {
						if owner.NodeID == nodeID {
							nodeIdx = i
							break
						}
					}

					if nodeIdx > -1 {
						// Data node owns shard, so relinquish ownership
						// and set new owners on the shard.
						s.Owners = append(s.Owners[:nodeIdx], s.Owners[nodeIdx+1:]...)
						data.Databases[dbidx].RetentionPolicies[rpidx].ShardGroups[sgidx].Shards[found].Owners = s.Owners
					}

					// Shard no longer owned. Will need removing the shard.
					if len(s.Owners) == 0 {
						shards := sg.Shards
						data.Databases[dbidx].RetentionPolicies[rpidx].ShardGroups[sgidx].Shards = append(shards[:found], shards[found+1:]...)

						if len(shards) == 1 {
							// We just deleted the last shard in the shard group.
							data.Databases[dbidx].RetentionPolicies[rpidx].ShardGroups[sgidx].DeletedAt = time.Now()
						}
					}

					return
				}
			}
		}
	}
}

// ShardGroups returns a list of all shard groups on a database and retention policy.
func (data *Data) ShardGroups(database, policy string) ([]ShardGroupInfo, error) {
	// Find retention policy.
	rpi, err := data.RetentionPolicy(database, policy)
	if err != nil {
		return nil, err
	} else if rpi == nil {
		return nil, influxdb.ErrRetentionPolicyNotFound(policy)
	}
	groups := make([]ShardGroupInfo, 0, len(rpi.ShardGroups))
	for _, g := range rpi.ShardGroups {
		if g.Deleted() {
			continue
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// ShardGroupsByTimeRange returns a list of all shard groups on a database and policy that may contain data
// for the specified time range. Shard groups are sorted by start time.
func (data *Data) ShardGroupsByTimeRange(database, policy string, tmin, tmax time.Time) ([]ShardGroupInfo, error) {
	// Find retention policy.
	rpi, err := data.RetentionPolicy(database, policy)
	if err != nil {
		return nil, err
	} else if rpi == nil {
		return nil, influxdb.ErrRetentionPolicyNotFound(policy)
	}
	groups := make([]ShardGroupInfo, 0, len(rpi.ShardGroups))
	for _, g := range rpi.ShardGroups {
		if g.Deleted() || !g.Overlaps(tmin, tmax) {
			continue
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// ShardGroupByTimestamp returns the shard group on a database and policy for a given timestamp.
func (data *Data) ShardGroupByTimestamp(database, policy string, timestamp time.Time) (*ShardGroupInfo, error) {
	// Find retention policy.
	rpi, err := data.RetentionPolicy(database, policy)
	if err != nil {
		return nil, err
	} else if rpi == nil {
		return nil, influxdb.ErrRetentionPolicyNotFound(policy)
	}

	return rpi.ShardGroupByTimestamp(timestamp), nil
}

// CreateShardGroup creates a shard group on a database and policy for a given timestamp.
func (data *Data) CreateShardGroup(database, policy string, timestamp time.Time) error {
	// Ensure there are nodes in the metadata.
	if len(data.DataNodes) == 0 {
		return nil
	}

	// Find retention policy.
	rpi, err := data.RetentionPolicy(database, policy)
	if err != nil {
		return err
	} else if rpi == nil {
		return influxdb.ErrRetentionPolicyNotFound(policy)
	}

	// Verify that shard group doesn't already exist for this timestamp.
	if rpi.ShardGroupByTimestamp(timestamp) != nil {
		return nil
	}

	// Require at least one replica but no more replicas than nodes.
	replicaN := rpi.ReplicaN
	if replicaN == 0 {
		replicaN = 1
	} else if replicaN > len(data.DataNodes) {
		replicaN = len(data.DataNodes)
	}

	// Determine shard count by the least common multiple of node count and
	// replication factor divided by node count.
	// This will ensure nodes will get distributed across nodes evenly and
	// replicated the correct number of times.
	shardN := 1
	for shardN*replicaN%len(data.DataNodes) != 0 {
		shardN++
	}

	startTime := timestamp.Truncate(rpi.ShardGroupDuration).UTC()
	endTime := startTime.Add(rpi.ShardGroupDuration).UTC()
	if endTime.After(time.Unix(0, models.MaxNanoTime)) {
		// Shard group range is [start, end) so add one to the max time.
		endTime = time.Unix(0, models.MaxNanoTime+1)
	}

	for i := range rpi.ShardGroups {
		if rpi.ShardGroups[i].Deleted() {
			continue
		}
		startI := rpi.ShardGroups[i].StartTime
		endI := rpi.ShardGroups[i].EndTime
		if rpi.ShardGroups[i].Truncated() {
			endI = rpi.ShardGroups[i].TruncatedAt
		}

		// shard_i covers range [start_i, end_i)
		// We want the largest range [startTime, endTime) such that all of the following hold:
		//   startTime <= timestamp < endTime
		//   for all i, not { start_i < endTime && startTime < end_i }
		// Assume the above conditions are true for shards index < i, we want to modify startTime,endTime so they are true
		// also for shard_i

		// It must be the case that either endI <= timestamp || timestamp < startI, because otherwise:
		// startI <= timestamp < endI means timestamp is contained in shard I
		if !timestamp.Before(endI) && endI.After(startTime) {
			// startTime < endI <= timestamp
			startTime = endI
		}
		if startI.After(timestamp) && startI.Before(endTime) {
			// timestamp < startI < endTime
			endTime = startI
		}
	}

	// Create the shard group.
	data.MaxShardGroupID++
	sgi := ShardGroupInfo{}
	sgi.ID = data.MaxShardGroupID
	sgi.StartTime = startTime
	sgi.EndTime = endTime

	// Create shards on the group.
	sgi.Shards = make([]ShardInfo, shardN)
	for i := range sgi.Shards {
		data.MaxShardID++
		sgi.Shards[i] = ShardInfo{ID: data.MaxShardID}
	}

	// Assign data nodes to shards via round robin.
	// Start from a repeatably "random" place in the node list.
	nodeIndex := int(data.Index % uint64(len(data.DataNodes)))
	for i := range sgi.Shards {
		si := &sgi.Shards[i]
		for j := 0; j < replicaN; j++ {
			nodeID := data.DataNodes[nodeIndex%len(data.DataNodes)].ID
			si.Owners = append(si.Owners, ShardOwner{NodeID: nodeID})
			nodeIndex++
		}
	}

	// Retention policy has a new shard group, so update the policy. Shard
	// Groups must be stored in sorted order, as other parts of the system
	// assume this to be the case.
	rpi.ShardGroups = append(rpi.ShardGroups, sgi)
	sort.Sort(ShardGroupInfos(rpi.ShardGroups))

	return nil
}

// DeleteShardGroup removes a shard group from a database and retention policy by id.
func (data *Data) DeleteShardGroup(database, policy string, id uint64) error {
	// Find retention policy.
	rpi, err := data.RetentionPolicy(database, policy)
	if err != nil {
		return err
	} else if rpi == nil {
		return influxdb.ErrRetentionPolicyNotFound(policy)
	}

	// Find shard group by ID and set its deletion timestamp.
	for i := range rpi.ShardGroups {
		if rpi.ShardGroups[i].ID == id {
			rpi.ShardGroups[i].DeletedAt = time.Now().UTC()
			return nil
		}
	}

	return ErrShardGroupNotFound
}

// CreateContinuousQuery adds a named continuous query to a database.
func (data *Data) CreateContinuousQuery(database, name, query string) error {
	di := data.Database(database)
	if di == nil {
		return influxdb.ErrDatabaseNotFound(database)
	}

	// Ensure the name doesn't already exist.
	for _, cq := range di.ContinuousQueries {
		if cq.Name == name {
			// If the query string is the same, we'll silently return,
			// otherwise we'll assume the user might be trying to
			// overwrite an existing CQ with a different query.
			if strings.ToLower(cq.Query) == strings.ToLower(query) {
				return nil
			}
			return ErrContinuousQueryExists
		}
	}

	// Append new query.
	di.ContinuousQueries = append(di.ContinuousQueries, ContinuousQueryInfo{
		Name:  name,
		Query: query,
	})

	return nil
}

// DropContinuousQuery removes a continuous query.
func (data *Data) DropContinuousQuery(database, name string) error {
	di := data.Database(database)
	if di == nil {
		return nil
	}

	for i := range di.ContinuousQueries {
		if di.ContinuousQueries[i].Name == name {
			di.ContinuousQueries = append(di.ContinuousQueries[:i], di.ContinuousQueries[i+1:]...)
			return nil
		}
	}
	return nil
}

// validateURL returns an error if the URL does not have a port or uses a scheme other than UDP or HTTP.
func validateURL(input string) error {
	u, err := url.Parse(input)
	if err != nil {
		return ErrInvalidSubscriptionURL(input)
	}

	if u.Scheme != "udp" && u.Scheme != "http" && u.Scheme != "https" {
		return ErrInvalidSubscriptionURL(input)
	}

	_, port, err := net.SplitHostPort(u.Host)
	if err != nil || port == "" {
		return ErrInvalidSubscriptionURL(input)
	}

	return nil
}

// CreateSubscription adds a named subscription to a database and retention policy.
func (data *Data) CreateSubscription(database, rp, name, mode string, destinations []string) error {
	for _, d := range destinations {
		if err := validateURL(d); err != nil {
			return err
		}
	}

	rpi, err := data.RetentionPolicy(database, rp)
	if err != nil {
		return err
	} else if rpi == nil {
		return influxdb.ErrRetentionPolicyNotFound(rp)
	}

	// Ensure the name doesn't already exist.
	for i := range rpi.Subscriptions {
		if rpi.Subscriptions[i].Name == name {
			return ErrSubscriptionExists
		}
	}

	// Append new query.
	rpi.Subscriptions = append(rpi.Subscriptions, SubscriptionInfo{
		Name:         name,
		Mode:         mode,
		Destinations: destinations,
	})

	return nil
}

// DropSubscription removes a subscription.
func (data *Data) DropSubscription(database, rp, name string) error {
	rpi, err := data.RetentionPolicy(database, rp)
	if err != nil {
		return err
	} else if rpi == nil {
		return influxdb.ErrRetentionPolicyNotFound(rp)
	}

	for i := range rpi.Subscriptions {
		if rpi.Subscriptions[i].Name == name {
			rpi.Subscriptions = append(rpi.Subscriptions[:i], rpi.Subscriptions[i+1:]...)
			return nil
		}
	}
	return ErrSubscriptionNotFound
}

func (data *Data) user(username string) *UserInfo {
	for i := range data.Users {
		if data.Users[i].Name == username {
			return &data.Users[i]
		}
	}
	return nil
}

// User returns a user by username.
func (data *Data) User(username string) User {
	u := data.user(username)
	if u == nil {
		// prevent non-nil interface with nil pointer
		return nil
	}
	return u
}

// CreateUser creates a new user.
func (data *Data) CreateUser(name, hash string, admin bool) error {
	// Ensure the user doesn't already exist.
	if name == "" {
		return ErrUsernameRequired
	} else if data.User(name) != nil {
		return ErrUserExists
	}

	// Append new user.
	data.Users = append(data.Users, UserInfo{
		Name:  name,
		Hash:  hash,
		Admin: admin,
	})

	// We know there is now at least one admin user.
	if admin {
		data.adminUserExists = true
	}

	return nil
}

// DropUser removes an existing user by name.
func (data *Data) DropUser(name string) error {
	for i := range data.Users {
		if data.Users[i].Name == name {
			wasAdmin := data.Users[i].Admin
			data.Users = append(data.Users[:i], data.Users[i+1:]...)

			// Maybe we dropped the only admin user?
			if wasAdmin {
				data.adminUserExists = data.hasAdminUser()
			}
			return nil
		}
	}

	return ErrUserNotFound
}

// UpdateUser updates the password hash of an existing user.
func (data *Data) UpdateUser(name, hash string) error {
	for i := range data.Users {
		if data.Users[i].Name == name {
			data.Users[i].Hash = hash
			return nil
		}
	}
	return ErrUserNotFound
}

// CloneUsers returns a copy of the user infos.
func (data *Data) CloneUsers() []UserInfo {
	if len(data.Users) == 0 {
		return []UserInfo{}
	}
	users := make([]UserInfo, len(data.Users))
	for i := range data.Users {
		users[i] = data.Users[i].clone()
	}

	return users
}

// SetPrivilege sets a privilege for a user on a database.
func (data *Data) SetPrivilege(name, database string, p influxql.Privilege) error {
	ui := data.user(name)
	if ui == nil {
		return ErrUserNotFound
	}

	if data.Database(database) == nil {
		return influxdb.ErrDatabaseNotFound(database)
	}

	if ui.Privileges == nil {
		ui.Privileges = make(map[string]influxql.Privilege)
	}
	ui.Privileges[database] = p

	return nil
}

// SetAdminPrivilege sets the admin privilege for a user.
func (data *Data) SetAdminPrivilege(name string, admin bool) error {
	ui := data.user(name)
	if ui == nil {
		return ErrUserNotFound
	}

	ui.Admin = admin

	// We could have promoted or revoked the only admin. Check if an admin
	// user exists.
	data.adminUserExists = data.hasAdminUser()
	return nil
}

// AdminUserExists returns true if an admin user exists.
func (data Data) AdminUserExists() bool {
	return data.adminUserExists
}

// UserPrivileges gets the privileges for a user.
func (data *Data) UserPrivileges(name string) (map[string]influxql.Privilege, error) {
	ui := data.user(name)
	if ui == nil {
		return nil, ErrUserNotFound
	}

	return ui.Privileges, nil
}

// UserPrivilege gets the privilege for a user on a database.
func (data *Data) UserPrivilege(name, database string) (*influxql.Privilege, error) {
	ui := data.user(name)
	if ui == nil {
		return nil, ErrUserNotFound
	}

	for db, p := range ui.Privileges {
		if db == database {
			return &p, nil
		}
	}

	return influxql.NewPrivilege(influxql.NoPrivileges), nil
}

// Clone returns a copy of data with a new version.
func (data *Data) Clone() *Data {
	other := *data

	other.Databases = data.CloneDatabases()
	other.Users = data.CloneUsers()

	return &other
}

// marshal serializes to a protobuf representation.
func (data *Data) marshal() *internal.Data {
	pb := &internal.Data{
		Term:      proto.Uint64(data.Term),
		Index:     proto.Uint64(data.Index),
		ClusterID: proto.Uint64(data.ClusterID),

		MaxNodeID:       proto.Uint64(data.MaxNodeID),
		MaxShardGroupID: proto.Uint64(data.MaxShardGroupID),
		MaxShardID:      proto.Uint64(data.MaxShardID),
	}

	pb.DataNodes = make([]*internal.NodeInfo, len(data.DataNodes))
	for i := range data.DataNodes {
		pb.DataNodes[i] = data.DataNodes[i].marshal()
	}

	pb.MetaNodes = make([]*internal.NodeInfo, len(data.MetaNodes))
	for i := range data.MetaNodes {
		pb.MetaNodes[i] = data.MetaNodes[i].marshal()
	}

	pb.Databases = make([]*internal.DatabaseInfo, len(data.Databases))
	for i := range data.Databases {
		pb.Databases[i] = data.Databases[i].marshal()
	}

	pb.Users = make([]*internal.UserInfo, len(data.Users))
	for i := range data.Users {
		pb.Users[i] = data.Users[i].marshal()
	}

	return pb
}

// unmarshal deserializes from a protobuf representation.
func (data *Data) unmarshal(pb *internal.Data) {
	data.Term = pb.GetTerm()
	data.Index = pb.GetIndex()
	data.ClusterID = pb.GetClusterID()

	data.MaxNodeID = pb.GetMaxNodeID()
	data.MaxShardGroupID = pb.GetMaxShardGroupID()
	data.MaxShardID = pb.GetMaxShardID()

	// TODO: Nodes is deprecated. This is being left here to make migration from 0.9.x to 0.10.0 possible
	if len(pb.GetNodes()) > 0 {
		data.DataNodes = make([]NodeInfo, len(pb.GetNodes()))
		for i, x := range pb.GetNodes() {
			data.DataNodes[i].unmarshal(x)
		}
	} else {
		data.DataNodes = make([]NodeInfo, len(pb.GetDataNodes()))
		for i, x := range pb.GetDataNodes() {
			data.DataNodes[i].unmarshal(x)
		}
	}

	data.MetaNodes = make([]NodeInfo, len(pb.GetMetaNodes()))
	for i, x := range pb.GetMetaNodes() {
		data.MetaNodes[i].unmarshal(x)
	}

	data.Databases = make([]DatabaseInfo, len(pb.GetDatabases()))
	for i, x := range pb.GetDatabases() {
		data.Databases[i].unmarshal(x)
	}

	data.Users = make([]UserInfo, len(pb.GetUsers()))
	for i, x := range pb.GetUsers() {
		data.Users[i].unmarshal(x)
	}

	// Exhaustively determine if there is an admin user. The marshalled cache
	// value may not be correct.
	data.adminUserExists = data.hasAdminUser()
}

// MarshalBinary encodes the metadata to a binary format.
func (data *Data) MarshalBinary() ([]byte, error) {
	return proto.Marshal(data.marshal())
}

// UnmarshalBinary decodes the object from a binary format.
func (data *Data) UnmarshalBinary(buf []byte) error {
	var pb internal.Data
	if err := proto.Unmarshal(buf, &pb); err != nil {
		return err
	}
	data.unmarshal(&pb)
	return nil
}

// TruncateShardGroups truncates any shard group that could contain timestamps beyond t.
func (data *Data) TruncateShardGroups(t time.Time) {
	for i := range data.Databases {
		dbi := &data.Databases[i]

		for j := range dbi.RetentionPolicies {
			rpi := &dbi.RetentionPolicies[j]

			for k := range rpi.ShardGroups {
				sgi := &rpi.ShardGroups[k]

				if !t.Before(sgi.EndTime) || sgi.Deleted() || (sgi.Truncated() && sgi.TruncatedAt.Before(t)) {
					continue
				}

				if !t.After(sgi.StartTime) {
					// future shardgroup
					sgi.TruncatedAt = sgi.StartTime
				} else {
					sgi.TruncatedAt = t
				}
			}
		}
	}
}

// PruneShardGroups remove deleted shard groups from the data store.
func (data *Data) PruneShardGroups() {
	expiration := time.Now().Add(ShardGroupDeletedExpiration)
	for i, d := range data.Databases {
		for j, rp := range d.RetentionPolicies {
			var changed bool
			var remainingShardGroups []ShardGroupInfo
			for _, sgi := range rp.ShardGroups {
				if sgi.DeletedAt.IsZero() || !expiration.After(sgi.DeletedAt) {
					remainingShardGroups = append(remainingShardGroups, sgi)
					continue
				}
				changed = true
			}
			if !changed {
				continue
			}
			data.Databases[i].RetentionPolicies[j].ShardGroups = remainingShardGroups
		}
	}
}

// hasAdminUser exhaustively checks for the presence of at least one admin
// user.
func (data *Data) hasAdminUser() bool {
	for _, u := range data.Users {
		if u.Admin {
			return true
		}
	}
	return false
}

// ImportData imports selected data into the current metadata.
// if non-empty, backupDBName, restoreDBName, backupRPName, restoreRPName can be used to select DB metadata from other,
// and to assign a new name to the imported data.  Returns a map of shard ID's in the old metadata to new shard ID's
// in the new metadata, along with a list of new databases created, both of which can assist in the import of existing
// shard data during a database restore.
func (data *Data) ImportData(other Data, backupDBName, restoreDBName, backupRPName, restoreRPName string) (map[uint64]uint64, []string, error) {
	shardIDMap := make(map[uint64]uint64)
	if backupDBName != "" {
		dbName, err := data.importOneDB(other, backupDBName, restoreDBName, backupRPName, restoreRPName, shardIDMap)
		if err != nil {
			return nil, nil, err
		}

		return shardIDMap, []string{dbName}, nil
	}

	// if no backupDBName then we'll try to import all the DB's.  If one of them fails, we'll mark the whole
	// operation a failure and return an error.
	var newDBs []string
	for _, dbi := range other.Databases {
		if dbi.Name == "_internal" {
			continue
		}
		dbName, err := data.importOneDB(other, dbi.Name, "", "", "", shardIDMap)
		if err != nil {
			return nil, nil, err
		}
		newDBs = append(newDBs, dbName)
	}
	return shardIDMap, newDBs, nil
}

// importOneDB imports a single database/rp from an external metadata object, renaming them if new names are provided.
func (data *Data) importOneDB(other Data, backupDBName, restoreDBName, backupRPName, restoreRPName string, shardIDMap map[uint64]uint64) (string, error) {

	dbPtr := other.Database(backupDBName)
	if dbPtr == nil {
		return "", fmt.Errorf("imported metadata does not have datbase named %s", backupDBName)
	}

	if restoreDBName == "" {
		restoreDBName = backupDBName
	}

	if data.Database(restoreDBName) != nil {
		return "", errors.New("database already exists")
	}

	// change the names if we want/need to
	err := data.CreateDatabase(restoreDBName)
	if err != nil {
		return "", err
	}
	dbImport := data.Database(restoreDBName)

	if backupRPName != "" {
		rpPtr := dbPtr.RetentionPolicy(backupRPName)

		if rpPtr != nil {
			rpImport := rpPtr.clone()
			if restoreRPName == "" {
				restoreRPName = backupRPName
			}
			rpImport.Name = restoreRPName
			dbImport.RetentionPolicies = []RetentionPolicyInfo{rpImport}
			dbImport.DefaultRetentionPolicy = restoreRPName
		} else {
			return "", fmt.Errorf("retention Policy not found in meta backup: %s.%s", backupDBName, backupRPName)
		}

	} else { // import all RP's without renaming
		dbImport.DefaultRetentionPolicy = dbPtr.DefaultRetentionPolicy
		if dbPtr.RetentionPolicies != nil {
			dbImport.RetentionPolicies = make([]RetentionPolicyInfo, len(dbPtr.RetentionPolicies))
			for i := range dbPtr.RetentionPolicies {
				dbImport.RetentionPolicies[i] = dbPtr.RetentionPolicies[i].clone()
			}
		}

	}

	// renumber the shard groups and shards for the new retention policy(ies)
	for _, rpImport := range dbImport.RetentionPolicies {
		for j, sgImport := range rpImport.ShardGroups {
			data.MaxShardGroupID++
			rpImport.ShardGroups[j].ID = data.MaxShardGroupID
			for k := range sgImport.Shards {
				data.MaxShardID++
				shardIDMap[sgImport.Shards[k].ID] = data.MaxShardID
				sgImport.Shards[k].ID = data.MaxShardID
				// OSS doesn't use Owners but if we are importing this from Enterprise, we'll want to clear it out
				// to avoid any issues if they ever export this DB again to bring back to Enterprise.
				sgImport.Shards[k].Owners = []ShardOwner{}
			}
		}
	}

	return restoreDBName, nil
}

// NodeInfo represents information about a single node in the cluster.
type NodeInfo struct {
	ID      uint64
	Addr    string
	TCPAddr string
}

// clone returns a deep copy of ni.
func (ni NodeInfo) clone() NodeInfo { return ni }

// marshal serializes to a protobuf representation.
func (ni NodeInfo) marshal() *internal.NodeInfo {
	pb := &internal.NodeInfo{}
	pb.ID = proto.Uint64(ni.ID)
	pb.Addr = proto.String(ni.Addr)
	pb.TCPAddr = proto.String(ni.TCPAddr)
	return pb
}

// unmarshal deserializes from a protobuf representation.
func (ni *NodeInfo) unmarshal(pb *internal.NodeInfo) {
	ni.ID = pb.GetID()
	ni.Addr = pb.GetAddr()
	ni.TCPAddr = pb.GetTCPAddr()
}

// NodeInfos is a slice of NodeInfo used for sorting
type NodeInfos []NodeInfo

// Len implements sort.Interface.
func (n NodeInfos) Len() int { return len(n) }

// Swap implements sort.Interface.
func (n NodeInfos) Swap(i, j int) { n[i], n[j] = n[j], n[i] }

// Less implements sort.Interface.
func (n NodeInfos) Less(i, j int) bool { return n[i].ID < n[j].ID }

// DatabaseInfo represents information about a database in the system.
type DatabaseInfo struct {
	Name                   string
	DefaultRetentionPolicy string
	RetentionPolicies      []RetentionPolicyInfo
	ContinuousQueries      []ContinuousQueryInfo
}

// RetentionPolicy returns a retention policy by name.
func (di DatabaseInfo) RetentionPolicy(name string) *RetentionPolicyInfo {
	if name == "" {
		if di.DefaultRetentionPolicy == "" {
			return nil
		}
		name = di.DefaultRetentionPolicy
	}

	for i := range di.RetentionPolicies {
		if di.RetentionPolicies[i].Name == name {
			return &di.RetentionPolicies[i]
		}
	}
	return nil
}

// ShardInfos returns a list of all shards' info for the database.
func (di DatabaseInfo) ShardInfos() []ShardInfo {
	shards := map[uint64]*ShardInfo{}
	for i := range di.RetentionPolicies {
		for j := range di.RetentionPolicies[i].ShardGroups {
			sg := di.RetentionPolicies[i].ShardGroups[j]
			// Skip deleted shard groups
			if sg.Deleted() {
				continue
			}
			for k := range sg.Shards {
				si := &di.RetentionPolicies[i].ShardGroups[j].Shards[k]
				shards[si.ID] = si
			}
		}
	}

	infos := make([]ShardInfo, 0, len(shards))
	for _, info := range shards {
		infos = append(infos, *info)
	}

	return infos
}

// clone returns a deep copy of di.
func (di DatabaseInfo) clone() DatabaseInfo {
	other := di

	if di.RetentionPolicies != nil {
		other.RetentionPolicies = make([]RetentionPolicyInfo, len(di.RetentionPolicies))
		for i := range di.RetentionPolicies {
			other.RetentionPolicies[i] = di.RetentionPolicies[i].clone()
		}
	}

	// Copy continuous queries.
	if di.ContinuousQueries != nil {
		other.ContinuousQueries = make([]ContinuousQueryInfo, len(di.ContinuousQueries))
		for i := range di.ContinuousQueries {
			other.ContinuousQueries[i] = di.ContinuousQueries[i].clone()
		}
	}

	return other
}

// marshal serializes to a protobuf representation.
func (di DatabaseInfo) marshal() *internal.DatabaseInfo {
	pb := &internal.DatabaseInfo{}
	pb.Name = proto.String(di.Name)
	pb.DefaultRetentionPolicy = proto.String(di.DefaultRetentionPolicy)

	pb.RetentionPolicies = make([]*internal.RetentionPolicyInfo, len(di.RetentionPolicies))
	for i := range di.RetentionPolicies {
		pb.RetentionPolicies[i] = di.RetentionPolicies[i].marshal()
	}

	pb.ContinuousQueries = make([]*internal.ContinuousQueryInfo, len(di.ContinuousQueries))
	for i := range di.ContinuousQueries {
		pb.ContinuousQueries[i] = di.ContinuousQueries[i].marshal()
	}
	return pb
}

// unmarshal deserializes from a protobuf representation.
func (di *DatabaseInfo) unmarshal(pb *internal.DatabaseInfo) {
	di.Name = pb.GetName()
	di.DefaultRetentionPolicy = pb.GetDefaultRetentionPolicy()

	if len(pb.GetRetentionPolicies()) > 0 {
		di.RetentionPolicies = make([]RetentionPolicyInfo, len(pb.GetRetentionPolicies()))
		for i, x := range pb.GetRetentionPolicies() {
			di.RetentionPolicies[i].unmarshal(x)
		}
	}

	if len(pb.GetContinuousQueries()) > 0 {
		di.ContinuousQueries = make([]ContinuousQueryInfo, len(pb.GetContinuousQueries()))
		for i, x := range pb.GetContinuousQueries() {
			di.ContinuousQueries[i].unmarshal(x)
		}
	}
}

// RetentionPolicySpec represents the specification for a new retention policy.
type RetentionPolicySpec struct {
	Name               string
	ReplicaN           *int
	Duration           *time.Duration
	ShardGroupDuration time.Duration
}

// NewRetentionPolicyInfo creates a new retention policy info from the specification.
func (s *RetentionPolicySpec) NewRetentionPolicyInfo() *RetentionPolicyInfo {
	return DefaultRetentionPolicyInfo().Apply(s)
}

// Matches checks if this retention policy specification matches
// an existing retention policy.
func (s *RetentionPolicySpec) Matches(rpi *RetentionPolicyInfo) bool {
	if rpi == nil {
		return false
	} else if s.Name != "" && s.Name != rpi.Name {
		return false
	} else if s.Duration != nil && *s.Duration != rpi.Duration {
		return false
	} else if s.ReplicaN != nil && *s.ReplicaN != rpi.ReplicaN {
		return false
	}

	// Normalise ShardDuration before comparing to any existing retention policies.
	// Normalize with the retention policy info's duration instead of the spec
	// since they should be the same and we're performing a comparison.
	sgDuration := normalisedShardDuration(s.ShardGroupDuration, rpi.Duration)
	return sgDuration == rpi.ShardGroupDuration
}

// marshal serializes to a protobuf representation.
func (s *RetentionPolicySpec) marshal() *internal.RetentionPolicySpec {
	pb := &internal.RetentionPolicySpec{}
	if s.Name != "" {
		pb.Name = proto.String(s.Name)
	}
	if s.Duration != nil {
		pb.Duration = proto.Int64(int64(*s.Duration))
	}
	if s.ShardGroupDuration > 0 {
		pb.ShardGroupDuration = proto.Int64(int64(s.ShardGroupDuration))
	}
	if s.ReplicaN != nil {
		pb.ReplicaN = proto.Uint32(uint32(*s.ReplicaN))
	}
	return pb
}

// unmarshal deserializes from a protobuf representation.
func (s *RetentionPolicySpec) unmarshal(pb *internal.RetentionPolicySpec) {
	if pb.Name != nil {
		s.Name = pb.GetName()
	}
	if pb.Duration != nil {
		duration := time.Duration(pb.GetDuration())
		s.Duration = &duration
	}
	if pb.ShardGroupDuration != nil {
		s.ShardGroupDuration = time.Duration(pb.GetShardGroupDuration())
	}
	if pb.ReplicaN != nil {
		replicaN := int(pb.GetReplicaN())
		s.ReplicaN = &replicaN
	}
}

// MarshalBinary encodes RetentionPolicySpec to a binary format.
func (s *RetentionPolicySpec) MarshalBinary() ([]byte, error) {
	return proto.Marshal(s.marshal())
}

// UnmarshalBinary decodes RetentionPolicySpec from a binary format.
func (s *RetentionPolicySpec) UnmarshalBinary(data []byte) error {
	var pb internal.RetentionPolicySpec
	if err := proto.Unmarshal(data, &pb); err != nil {
		return err
	}
	s.unmarshal(&pb)
	return nil
}

// RetentionPolicyInfo represents metadata about a retention policy.
type RetentionPolicyInfo struct {
	Name               string
	ReplicaN           int
	Duration           time.Duration
	ShardGroupDuration time.Duration
	ShardGroups        []ShardGroupInfo
	Subscriptions      []SubscriptionInfo
}

// NewRetentionPolicyInfo returns a new instance of RetentionPolicyInfo
// with default replication and duration.
func NewRetentionPolicyInfo(name string) *RetentionPolicyInfo {
	return &RetentionPolicyInfo{
		Name:     name,
		ReplicaN: DefaultRetentionPolicyReplicaN,
		Duration: DefaultRetentionPolicyDuration,
	}
}

// DefaultRetentionPolicyInfo returns a new instance of RetentionPolicyInfo
// with default name, replication, and duration.
func DefaultRetentionPolicyInfo() *RetentionPolicyInfo {
	return NewRetentionPolicyInfo(DefaultRetentionPolicyName)
}

// Apply applies a specification to the retention policy info.
func (rpi *RetentionPolicyInfo) Apply(spec *RetentionPolicySpec) *RetentionPolicyInfo {
	rp := &RetentionPolicyInfo{
		Name:               rpi.Name,
		ReplicaN:           rpi.ReplicaN,
		Duration:           rpi.Duration,
		ShardGroupDuration: rpi.ShardGroupDuration,
	}
	if spec.Name != "" {
		rp.Name = spec.Name
	}
	if spec.ReplicaN != nil {
		rp.ReplicaN = *spec.ReplicaN
	}
	if spec.Duration != nil {
		rp.Duration = *spec.Duration
	}
	rp.ShardGroupDuration = normalisedShardDuration(spec.ShardGroupDuration, rp.Duration)
	return rp
}

// ShardGroupByTimestamp returns the shard group in the policy that contains the timestamp,
// or nil if no shard group matches.
func (rpi *RetentionPolicyInfo) ShardGroupByTimestamp(timestamp time.Time) *ShardGroupInfo {
	for i := range rpi.ShardGroups {
		sgi := &rpi.ShardGroups[i]
		if sgi.Contains(timestamp) && !sgi.Deleted() && (!sgi.Truncated() || timestamp.Before(sgi.TruncatedAt)) {
			return &rpi.ShardGroups[i]
		}
	}

	return nil
}

// ExpiredShardGroups returns the Shard Groups which are considered expired, for the given time.
func (rpi *RetentionPolicyInfo) ExpiredShardGroups(t time.Time) []*ShardGroupInfo {
	var groups = make([]*ShardGroupInfo, 0)
	for i := range rpi.ShardGroups {
		if rpi.ShardGroups[i].Deleted() {
			continue
		}
		if rpi.Duration != 0 && rpi.ShardGroups[i].EndTime.Add(rpi.Duration).Before(t) {
			groups = append(groups, &rpi.ShardGroups[i])
		}
	}
	return groups
}

// DeletedShardGroups returns the Shard Groups which are marked as deleted.
func (rpi *RetentionPolicyInfo) DeletedShardGroups() []*ShardGroupInfo {
	var groups = make([]*ShardGroupInfo, 0)
	for i := range rpi.ShardGroups {
		if rpi.ShardGroups[i].Deleted() {
			groups = append(groups, &rpi.ShardGroups[i])
		}
	}
	return groups
}

// marshal serializes to a protobuf representation.
func (rpi *RetentionPolicyInfo) marshal() *internal.RetentionPolicyInfo {
	pb := &internal.RetentionPolicyInfo{
		Name:               proto.String(rpi.Name),
		ReplicaN:           proto.Uint32(uint32(rpi.ReplicaN)),
		Duration:           proto.Int64(int64(rpi.Duration)),
		ShardGroupDuration: proto.Int64(int64(rpi.ShardGroupDuration)),
	}

	pb.ShardGroups = make([]*internal.ShardGroupInfo, len(rpi.ShardGroups))
	for i, sgi := range rpi.ShardGroups {
		pb.ShardGroups[i] = sgi.marshal()
	}

	pb.Subscriptions = make([]*internal.SubscriptionInfo, len(rpi.Subscriptions))
	for i, sub := range rpi.Subscriptions {
		pb.Subscriptions[i] = sub.marshal()
	}

	return pb
}

// unmarshal deserializes from a protobuf representation.
func (rpi *RetentionPolicyInfo) unmarshal(pb *internal.RetentionPolicyInfo) {
	rpi.Name = pb.GetName()
	rpi.ReplicaN = int(pb.GetReplicaN())
	rpi.Duration = time.Duration(pb.GetDuration())
	rpi.ShardGroupDuration = time.Duration(pb.GetShardGroupDuration())

	if len(pb.GetShardGroups()) > 0 {
		rpi.ShardGroups = make([]ShardGroupInfo, len(pb.GetShardGroups()))
		for i, x := range pb.GetShardGroups() {
			rpi.ShardGroups[i].unmarshal(x)
		}
	}
	if len(pb.GetSubscriptions()) > 0 {
		rpi.Subscriptions = make([]SubscriptionInfo, len(pb.GetSubscriptions()))
		for i, x := range pb.GetSubscriptions() {
			rpi.Subscriptions[i].unmarshal(x)
		}
	}
}

// clone returns a deep copy of rpi.
func (rpi RetentionPolicyInfo) clone() RetentionPolicyInfo {
	other := rpi

	if rpi.ShardGroups != nil {
		other.ShardGroups = make([]ShardGroupInfo, len(rpi.ShardGroups))
		for i := range rpi.ShardGroups {
			other.ShardGroups[i] = rpi.ShardGroups[i].clone()
		}
	}

	return other
}

// MarshalBinary encodes rpi to a binary format.
func (rpi *RetentionPolicyInfo) MarshalBinary() ([]byte, error) {
	return proto.Marshal(rpi.marshal())
}

// UnmarshalBinary decodes rpi from a binary format.
func (rpi *RetentionPolicyInfo) UnmarshalBinary(data []byte) error {
	var pb internal.RetentionPolicyInfo
	if err := proto.Unmarshal(data, &pb); err != nil {
		return err
	}
	rpi.unmarshal(&pb)
	return nil
}

// shardGroupDuration returns the default duration for a shard group based on a policy duration.
func shardGroupDuration(d time.Duration) time.Duration {
	if d >= 180*24*time.Hour || d == 0 { // 6 months or 0
		return 7 * 24 * time.Hour
	} else if d >= 2*24*time.Hour { // 2 days
		return 1 * 24 * time.Hour
	}
	return 1 * time.Hour
}

// normalisedShardDuration returns normalised shard duration based on a policy duration.
func normalisedShardDuration(sgd, d time.Duration) time.Duration {
	// If it is zero, it likely wasn't specified, so we default to the shard group duration
	if sgd == 0 {
		return shardGroupDuration(d)
	}
	// If it was specified, but it's less than the MinRetentionPolicyDuration, then normalize
	// to the MinRetentionPolicyDuration
	if sgd < MinRetentionPolicyDuration {
		return shardGroupDuration(MinRetentionPolicyDuration)
	}
	return sgd
}

// ShardGroupInfo represents metadata about a shard group. The DeletedAt field is important
// because it makes it clear that a ShardGroup has been marked as deleted, and allow the system
// to be sure that a ShardGroup is not simply missing. If the DeletedAt is set, the system can
// safely delete any associated shards.
type ShardGroupInfo struct {
	ID          uint64
	StartTime   time.Time
	EndTime     time.Time
	DeletedAt   time.Time
	Shards      []ShardInfo
	TruncatedAt time.Time
}

// ShardGroupInfos implements sort.Interface on []ShardGroupInfo, based
// on the StartTime field.
type ShardGroupInfos []ShardGroupInfo

// Len implements sort.Interface.
func (a ShardGroupInfos) Len() int { return len(a) }

// Swap implements sort.Interface.
func (a ShardGroupInfos) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

// Less implements sort.Interface.
func (a ShardGroupInfos) Less(i, j int) bool {
	iEnd := a[i].EndTime
	if a[i].Truncated() {
		iEnd = a[i].TruncatedAt
	}

	jEnd := a[j].EndTime
	if a[j].Truncated() {
		jEnd = a[j].TruncatedAt
	}

	if iEnd.Equal(jEnd) {
		return a[i].StartTime.Before(a[j].StartTime)
	}

	return iEnd.Before(jEnd)
}

// Contains returns true iif StartTime ≤ t < EndTime.
func (sgi *ShardGroupInfo) Contains(t time.Time) bool {
	return !t.Before(sgi.StartTime) && t.Before(sgi.EndTime)
}

// Overlaps returns whether the shard group contains data for the time range between min and max
func (sgi *ShardGroupInfo) Overlaps(min, max time.Time) bool {
	return !sgi.StartTime.After(max) && sgi.EndTime.After(min)
}

// Deleted returns whether this ShardGroup has been deleted.
func (sgi *ShardGroupInfo) Deleted() bool {
	return !sgi.DeletedAt.IsZero()
}

// Truncated returns true if this ShardGroup has been truncated (no new writes).
func (sgi *ShardGroupInfo) Truncated() bool {
	return !sgi.TruncatedAt.IsZero()
}

// clone returns a deep copy of sgi.
func (sgi ShardGroupInfo) clone() ShardGroupInfo {
	other := sgi

	if sgi.Shards != nil {
		other.Shards = make([]ShardInfo, len(sgi.Shards))
		for i := range sgi.Shards {
			other.Shards[i] = sgi.Shards[i].clone()
		}
	}

	return other
}

type hashIDer interface {
	HashID() uint64
}

// ShardFor returns the ShardInfo for a Point or other hashIDer.
func (sgi *ShardGroupInfo) ShardFor(p hashIDer) ShardInfo {
	if len(sgi.Shards) == 1 {
		return sgi.Shards[0]
	}
	return sgi.Shards[p.HashID()%uint64(len(sgi.Shards))]
}

// marshal serializes to a protobuf representation.
func (sgi *ShardGroupInfo) marshal() *internal.ShardGroupInfo {
	pb := &internal.ShardGroupInfo{
		ID:        proto.Uint64(sgi.ID),
		StartTime: proto.Int64(MarshalTime(sgi.StartTime)),
		EndTime:   proto.Int64(MarshalTime(sgi.EndTime)),
		DeletedAt: proto.Int64(MarshalTime(sgi.DeletedAt)),
	}

	if !sgi.TruncatedAt.IsZero() {
		pb.TruncatedAt = proto.Int64(MarshalTime(sgi.TruncatedAt))
	}

	pb.Shards = make([]*internal.ShardInfo, len(sgi.Shards))
	for i := range sgi.Shards {
		pb.Shards[i] = sgi.Shards[i].marshal()
	}

	return pb
}

// unmarshal deserializes from a protobuf representation.
func (sgi *ShardGroupInfo) unmarshal(pb *internal.ShardGroupInfo) {
	sgi.ID = pb.GetID()
	if i := pb.GetStartTime(); i == 0 {
		sgi.StartTime = time.Unix(0, 0).UTC()
	} else {
		sgi.StartTime = UnmarshalTime(i)
	}
	if i := pb.GetEndTime(); i == 0 {
		sgi.EndTime = time.Unix(0, 0).UTC()
	} else {
		sgi.EndTime = UnmarshalTime(i)
	}
	sgi.DeletedAt = UnmarshalTime(pb.GetDeletedAt())

	if pb != nil && pb.TruncatedAt != nil {
		sgi.TruncatedAt = UnmarshalTime(pb.GetTruncatedAt())
	}

	if len(pb.GetShards()) > 0 {
		sgi.Shards = make([]ShardInfo, len(pb.GetShards()))
		for i, x := range pb.GetShards() {
			sgi.Shards[i].unmarshal(x)
		}
	}
}

// ShardInfo represents metadata about a shard.
type ShardInfo struct {
	ID     uint64
	Owners []ShardOwner
}

// OwnedBy determines whether the shard's owner IDs includes nodeID.
func (si ShardInfo) OwnedBy(nodeID uint64) bool {
	for _, so := range si.Owners {
		if so.NodeID == nodeID {
			return true
		}
	}
	return false
}

// clone returns a deep copy of si.
func (si ShardInfo) clone() ShardInfo {
	other := si

	if si.Owners != nil {
		other.Owners = make([]ShardOwner, len(si.Owners))
		for i := range si.Owners {
			other.Owners[i] = si.Owners[i].clone()
		}
	}

	return other
}

// marshal serializes to a protobuf representation.
func (si ShardInfo) marshal() *internal.ShardInfo {
	pb := &internal.ShardInfo{
		ID: proto.Uint64(si.ID),
	}

	pb.Owners = make([]*internal.ShardOwner, len(si.Owners))
	for i := range si.Owners {
		pb.Owners[i] = si.Owners[i].marshal()
	}

	return pb
}

// UnmarshalBinary decodes the object from a binary format.
func (si *ShardInfo) UnmarshalBinary(buf []byte) error {
	var pb internal.ShardInfo
	if err := proto.Unmarshal(buf, &pb); err != nil {
		return err
	}
	si.unmarshal(&pb)
	return nil
}

// unmarshal deserializes from a protobuf representation.
func (si *ShardInfo) unmarshal(pb *internal.ShardInfo) {
	si.ID = pb.GetID()

	// If deprecated "OwnerIDs" exists then convert it to "Owners" format.
	if len(pb.GetOwnerIDs()) > 0 {
		si.Owners = make([]ShardOwner, len(pb.GetOwnerIDs()))
		for i, x := range pb.GetOwnerIDs() {
			si.Owners[i].unmarshal(&internal.ShardOwner{
				NodeID: proto.Uint64(x),
			})
		}
	} else if len(pb.GetOwners()) > 0 {
		si.Owners = make([]ShardOwner, len(pb.GetOwners()))
		for i, x := range pb.GetOwners() {
			si.Owners[i].unmarshal(x)
		}
	}
}

// SubscriptionInfo holds the subscription information.
type SubscriptionInfo struct {
	Name         string
	Mode         string
	Destinations []string
}

// marshal serializes to a protobuf representation.
func (si SubscriptionInfo) marshal() *internal.SubscriptionInfo {
	pb := &internal.SubscriptionInfo{
		Name: proto.String(si.Name),
		Mode: proto.String(si.Mode),
	}

	pb.Destinations = make([]string, len(si.Destinations))
	for i := range si.Destinations {
		pb.Destinations[i] = si.Destinations[i]
	}
	return pb
}

// unmarshal deserializes from a protobuf representation.
func (si *SubscriptionInfo) unmarshal(pb *internal.SubscriptionInfo) {
	si.Name = pb.GetName()
	si.Mode = pb.GetMode()

	if len(pb.GetDestinations()) > 0 {
		si.Destinations = make([]string, len(pb.GetDestinations()))
		copy(si.Destinations, pb.GetDestinations())
	}
}

// ShardOwner represents a node that owns a shard.
type ShardOwner struct {
	NodeID uint64
}

// clone returns a deep copy of so.
func (so ShardOwner) clone() ShardOwner {
	return so
}

// marshal serializes to a protobuf representation.
func (so ShardOwner) marshal() *internal.ShardOwner {
	return &internal.ShardOwner{
		NodeID: proto.Uint64(so.NodeID),
	}
}

// unmarshal deserializes from a protobuf representation.
func (so *ShardOwner) unmarshal(pb *internal.ShardOwner) {
	so.NodeID = pb.GetNodeID()
}

// ContinuousQueryInfo represents metadata about a continuous query.
type ContinuousQueryInfo struct {
	Name  string
	Query string
}

// clone returns a deep copy of cqi.
func (cqi ContinuousQueryInfo) clone() ContinuousQueryInfo { return cqi }

// marshal serializes to a protobuf representation.
func (cqi ContinuousQueryInfo) marshal() *internal.ContinuousQueryInfo {
	return &internal.ContinuousQueryInfo{
		Name:  proto.String(cqi.Name),
		Query: proto.String(cqi.Query),
	}
}

// unmarshal deserializes from a protobuf representation.
func (cqi *ContinuousQueryInfo) unmarshal(pb *internal.ContinuousQueryInfo) {
	cqi.Name = pb.GetName()
	cqi.Query = pb.GetQuery()
}

var _ query.FineAuthorizer = (*UserInfo)(nil)

// UserInfo represents metadata about a user in the system.
type UserInfo struct {
	// User's name.
	Name string

	// Hashed password.
	Hash string

	// Whether the user is an admin, i.e. allowed to do everything.
	Admin bool

	// Map of database name to granted privilege.
	Privileges map[string]influxql.Privilege
}

type User interface {
	query.FineAuthorizer
	ID() string
	AuthorizeUnrestricted() bool
}

func (u *UserInfo) ID() string {
	return u.Name
}

// AuthorizeDatabase returns true if the user is authorized for the given privilege on the given database.
func (ui *UserInfo) AuthorizeDatabase(privilege influxql.Privilege, database string) bool {
	if ui.Admin || privilege == influxql.NoPrivileges {
		return true
	}
	p, ok := ui.Privileges[database]
	return ok && (p == privilege || p == influxql.AllPrivileges)
}

// AuthorizeSeriesRead is used to limit access per-series (enterprise only)
func (u *UserInfo) AuthorizeSeriesRead(database string, measurement []byte, tags models.Tags) bool {
	return true
}

// AuthorizeSeriesWrite is used to limit access per-series (enterprise only)
func (u *UserInfo) AuthorizeSeriesWrite(database string, measurement []byte, tags models.Tags) bool {
	return true
}

// IsOpen is a method on FineAuthorizer to indicate all fine auth is permitted and short circuit some checks.
func (u *UserInfo) IsOpen() bool {
	return true
}

// AuthorizeUnrestricted identifies the admin user
//
// Only the pprof endpoint uses this, we should prefer to have explicit permissioning instead.
func (u *UserInfo) AuthorizeUnrestricted() bool {
	return u.Admin
}

// clone returns a deep copy of si.
func (ui UserInfo) clone() UserInfo {
	other := ui

	if ui.Privileges != nil {
		other.Privileges = make(map[string]influxql.Privilege)
		for k, v := range ui.Privileges {
			other.Privileges[k] = v
		}
	}

	return other
}

// marshal serializes to a protobuf representation.
func (ui UserInfo) marshal() *internal.UserInfo {
	pb := &internal.UserInfo{
		Name:  proto.String(ui.Name),
		Hash:  proto.String(ui.Hash),
		Admin: proto.Bool(ui.Admin),
	}

	for database, privilege := range ui.Privileges {
		pb.Privileges = append(pb.Privileges, &internal.UserPrivilege{
			Database:  proto.String(database),
			Privilege: proto.Int32(int32(privilege)),
		})
	}

	return pb
}

// unmarshal deserializes from a protobuf representation.
func (ui *UserInfo) unmarshal(pb *internal.UserInfo) {
	ui.Name = pb.GetName()
	ui.Hash = pb.GetHash()
	ui.Admin = pb.GetAdmin()

	ui.Privileges = make(map[string]influxql.Privilege)
	for _, p := range pb.GetPrivileges() {
		ui.Privileges[p.GetDatabase()] = influxql.Privilege(p.GetPrivilege())
	}
}

// Lease represents a lease held on a resource.
type Lease struct {
	Name       string    `json:"name"`
	Expiration time.Time `json:"expiration"`
	Owner      uint64    `json:"owner"`
}

// Leases is a concurrency-safe collection of leases keyed by name.
type Leases struct {
	mu sync.Mutex
	m  map[string]*Lease
	d  time.Duration
}

// NewLeases returns a new instance of Leases.
func NewLeases(d time.Duration) *Leases {
	return &Leases{
		m: make(map[string]*Lease),
		d: d,
	}
}

// Acquire acquires a lease with the given name for the given nodeID.
// If the lease doesn't exist or exists but is expired, a valid lease is returned.
// If nodeID already owns the named and unexpired lease, the lease expiration is extended.
// If a different node owns the lease, an error is returned.
func (leases *Leases) Acquire(name string, nodeID uint64) (*Lease, error) {
	leases.mu.Lock()
	defer leases.mu.Unlock()

	l := leases.m[name]
	if l != nil {
		if time.Now().After(l.Expiration) || l.Owner == nodeID {
			l.Expiration = time.Now().Add(leases.d)
			l.Owner = nodeID
			return l, nil
		}
		return l, errors.New("another node has the lease")
	}

	l = &Lease{
		Name:       name,
		Expiration: time.Now().Add(leases.d),
		Owner:      nodeID,
	}

	leases.m[name] = l

	return l, nil
}

// MarshalTime converts t to nanoseconds since epoch. A zero time returns 0.
func MarshalTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// UnmarshalTime converts nanoseconds since epoch to time.
// A zero value returns a zero time.
func UnmarshalTime(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, v).UTC()
}

// ValidName checks to see if the given name can would be valid for DB/RP name
func ValidName(name string) bool {
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return false
		}
	}

	return name != "" &&
		name != "." &&
		name != ".." &&
		!strings.ContainsAny(name, `/\`)
}

const (
	NodeTypeData = "data"
	NodeTypeMeta = "meta"
)

const (
	NodeStatusJoined    = "joined"
	NodeStatusDisjoined = "disjoined"
)

type Announcements map[string]*Announcement

func (a *Announcements) MarshalBinary() ([]byte, error) {
	return json.Marshal(a)
}

func (a *Announcements) UnmarshalBinary(buf []byte) error {
	return json.Unmarshal(buf, a)
}

type Announcement struct {
	TCPAddr    string    `json:"tcpAddr"`
	HTTPAddr   string    `json:"httpAddr"`
	HTTPScheme string    `json:"httpScheme"`
	Time       time.Time `json:"time"`
	NodeType   string    `json:"nodeType"`
	Status     string    `json:"status"`
	Context    Context   `json:"context"`
	Version    string    `json:"version"`
}

type Context map[string]json.RawMessage

type DataNodeStatus struct {
	NodeType   string   `json:"nodeType"`
	Hostname   string   `json:"hostname"`
	TCPBind    string   `json:"tcpBind"`
	TCPAddr    string   `json:"tcpAddr"`
	HTTPAddr   string   `json:"httpAddr"`
	NodeStatus string   `json:"nodeStatus"`
	MetaAddrs  []string `json:"metaAddrs"`
}

type MetaNodeStatus struct {
	NodeType string   `json:"nodeType"`
	Leader   string   `json:"leader"`
	HTTPAddr string   `json:"httpAddr"`
	RaftAddr string   `json:"raftAddr"`
	Peers    []string `json:"peers"`
}

type DataNodeInfo struct {
	ID         uint64 `json:"id"`
	TCPAddr    string `json:"tcpAddr"`
	HTTPAddr   string `json:"httpAddr"`
	HTTPScheme string `json:"httpScheme"`
	Status     string `json:"status"`
	Version    string `json:"version"`
}

func NewDataNodeInfo(n *NodeInfo) *DataNodeInfo {
	return &DataNodeInfo{
		ID:       n.ID,
		TCPAddr:  n.TCPAddr,
		HTTPAddr: n.Addr,
	}
}

type MetaNodeInfo struct {
	ID         uint64 `json:"id"`
	Addr       string `json:"addr"`
	HTTPScheme string `json:"httpScheme"`
	TCPAddr    string `json:"tcpAddr"`
	Version    string `json:"version"`
}

func NewMetaNodeInfo(n *NodeInfo) *MetaNodeInfo {
	return &MetaNodeInfo{
		ID:      n.ID,
		Addr:    n.Addr,
		TCPAddr: n.TCPAddr,
	}
}

type ClusterInfo struct {
	Data []*DataNodeInfo `json:"data"`
	Meta []*MetaNodeInfo `json:"meta"`
}

type ClusterShardInfo struct {
	ID              uint64            `json:"id"`
	Database        string            `json:"database"`
	RetentionPolicy string            `json:"retention-policy"`
	ReplicaN        int               `json:"replica-n"`
	ShardGroupID    uint64            `json:"shard-group-id"`
	StartTime       time.Time         `json:"start-time"`
	EndTime         time.Time         `json:"end-time"`
	ExpireTime      time.Time         `json:"expire-time"`
	TruncatedAt     time.Time         `json:"truncated-at"`
	Owners          []*ShardOwnerInfo `json:"owners"`
}

type ShardOwnerInfo struct {
	ID           uint64    `json:"id"`
	TCPAddr      string    `json:"tcpAddr"`
	State        string    `json:"state"`
	LastModified time.Time `json:"last-modified"`
	Size         int64     `json:"size"`
	Err          string    `json:"err"`
}

type UserPrivilege struct {
	Name     string `json:"name"`
	Hash     string `json:"hash,omitempty"`
	Password string `json:"password,omitempty"`
}

type UserPrivileges struct {
	Users []*UserPrivilege `json:"users,omitempty"`
}

type UserOperation struct {
	Action string         `json:"action"`
	User   *UserPrivilege `json:"user"`
}

type RolePrivilege struct {
	Name string `json:"name"`
}

type RolePrivileges struct {
	Roles []*RolePrivilege `json:"roles,omitempty"`
}

type RoleOperation struct {
	Action string         `json:"action"`
	Role   *RolePrivilege `json:"role"`
}
