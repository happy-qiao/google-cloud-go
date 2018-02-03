/*
Copyright 2015 Google Inc. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bigtable

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/bigtable/internal/gax"
	btopt "cloud.google.com/go/bigtable/internal/option"
	"cloud.google.com/go/longrunning"
	lroauto "cloud.google.com/go/longrunning/autogen"
	"github.com/golang/protobuf/ptypes"
	durpb "github.com/golang/protobuf/ptypes/duration"
	"golang.org/x/net/context"
	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	gtransport "google.golang.org/api/transport/grpc"
	btapb "google.golang.org/genproto/googleapis/bigtable/admin/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const adminAddr = "bigtableadmin.googleapis.com:443"

// AdminClient is a client type for performing admin operations within a specific instance.
type AdminClient struct {
	conn      *grpc.ClientConn
	tClient   btapb.BigtableTableAdminClient
	lroClient *lroauto.OperationsClient

	project, instance string

	// Metadata to be sent with each request.
	md metadata.MD
}

// NewAdminClient creates a new AdminClient for a given project and instance.
func NewAdminClient(ctx context.Context, project, instance string, opts ...option.ClientOption) (*AdminClient, error) {
	o, err := btopt.DefaultClientOptions(adminAddr, AdminScope, clientUserAgent)
	if err != nil {
		return nil, err
	}
	// Need to add scopes for long running operations (for create table & snapshots)
	o = append(o, option.WithScopes(cloudresourcemanager.CloudPlatformScope))
	o = append(o, opts...)
	conn, err := gtransport.Dial(ctx, o...)
	if err != nil {
		return nil, fmt.Errorf("dialing: %v", err)
	}

	lroClient, err := lroauto.NewOperationsClient(ctx, option.WithGRPCConn(conn))
	if err != nil {
		// This error "should not happen", since we are just reusing old connection
		// and never actually need to dial.
		// If this does happen, we could leak conn. However, we cannot close conn:
		// If the user invoked the function with option.WithGRPCConn,
		// we would close a connection that's still in use.
		// TODO(pongad): investigate error conditions.
		return nil, err
	}

	return &AdminClient{
		conn:      conn,
		tClient:   btapb.NewBigtableTableAdminClient(conn),
		lroClient: lroClient,
		project:   project,
		instance:  instance,
		md:        metadata.Pairs(resourcePrefixHeader, fmt.Sprintf("projects/%s/instances/%s", project, instance)),
	}, nil
}

// Close closes the AdminClient.
func (ac *AdminClient) Close() error {
	return ac.conn.Close()
}

func (ac *AdminClient) instancePrefix() string {
	return fmt.Sprintf("projects/%s/instances/%s", ac.project, ac.instance)
}

// Tables returns a list of the tables in the instance.
func (ac *AdminClient) Tables(ctx context.Context) ([]string, error) {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	req := &btapb.ListTablesRequest{
		Parent: prefix,
	}
	res, err := ac.tClient.ListTables(ctx, req)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(res.Tables))
	for _, tbl := range res.Tables {
		names = append(names, strings.TrimPrefix(tbl.Name, prefix+"/tables/"))
	}
	return names, nil
}

// TableConf contains all of the information necessary to create a table with column families.
type TableConf struct {
	TableID   string
	SplitKeys []string
	// Families is a map from family name to GCPolicy
	Families map[string]GCPolicy
}

// CreateTable creates a new table in the instance.
// This method may return before the table's creation is complete.
func (ac *AdminClient) CreateTable(ctx context.Context, table string) error {
	return ac.CreateTableFromConf(ctx, &TableConf{TableID: table})
}

// CreatePresplitTable creates a new table in the instance.
// The list of row keys will be used to initially split the table into multiple tablets.
// Given two split keys, "s1" and "s2", three tablets will be created,
// spanning the key ranges: [, s1), [s1, s2), [s2, ).
// This method may return before the table's creation is complete.
func (ac *AdminClient) CreatePresplitTable(ctx context.Context, table string, splitKeys []string) error {
	return ac.CreateTableFromConf(ctx, &TableConf{TableID: table, SplitKeys: splitKeys})
}

// CreateTableFromConf creates a new table in the instance from the given configuration.
func (ac *AdminClient) CreateTableFromConf(ctx context.Context, conf *TableConf) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	var req_splits []*btapb.CreateTableRequest_Split
	for _, split := range conf.SplitKeys {
		req_splits = append(req_splits, &btapb.CreateTableRequest_Split{[]byte(split)})
	}
	var tbl btapb.Table
	if conf.Families != nil {
		tbl.ColumnFamilies = make(map[string]*btapb.ColumnFamily)
		for fam, policy := range conf.Families {
			tbl.ColumnFamilies[fam] = &btapb.ColumnFamily{policy.proto()}
		}
	}
	prefix := ac.instancePrefix()
	req := &btapb.CreateTableRequest{
		Parent:        prefix,
		TableId:       conf.TableID,
		Table:         &tbl,
		InitialSplits: req_splits,
	}
	_, err := ac.tClient.CreateTable(ctx, req)
	return err
}

// CreateColumnFamily creates a new column family in a table.
func (ac *AdminClient) CreateColumnFamily(ctx context.Context, table, family string) error {
	// TODO(dsymonds): Permit specifying gcexpr and any other family settings.
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	req := &btapb.ModifyColumnFamiliesRequest{
		Name: prefix + "/tables/" + table,
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{{
			Id:  family,
			Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{&btapb.ColumnFamily{}},
		}},
	}
	_, err := ac.tClient.ModifyColumnFamilies(ctx, req)
	return err
}

// DeleteTable deletes a table and all of its data.
func (ac *AdminClient) DeleteTable(ctx context.Context, table string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	req := &btapb.DeleteTableRequest{
		Name: prefix + "/tables/" + table,
	}
	_, err := ac.tClient.DeleteTable(ctx, req)
	return err
}

// DeleteColumnFamily deletes a column family in a table and all of its data.
func (ac *AdminClient) DeleteColumnFamily(ctx context.Context, table, family string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	req := &btapb.ModifyColumnFamiliesRequest{
		Name: prefix + "/tables/" + table,
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{{
			Id:  family,
			Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Drop{true},
		}},
	}
	_, err := ac.tClient.ModifyColumnFamilies(ctx, req)
	return err
}

// TableInfo represents information about a table.
type TableInfo struct {
	// DEPRECATED - This field is deprecated. Please use FamilyInfos instead.
	Families    []string
	FamilyInfos []FamilyInfo
}

// FamilyInfo represents information about a column family.
type FamilyInfo struct {
	Name     string
	GCPolicy string
}

// TableInfo retrieves information about a table.
func (ac *AdminClient) TableInfo(ctx context.Context, table string) (*TableInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	req := &btapb.GetTableRequest{
		Name: prefix + "/tables/" + table,
	}
	res, err := ac.tClient.GetTable(ctx, req)
	if err != nil {
		return nil, err
	}
	ti := &TableInfo{}
	for name, fam := range res.ColumnFamilies {
		ti.Families = append(ti.Families, name)
		ti.FamilyInfos = append(ti.FamilyInfos, FamilyInfo{Name: name, GCPolicy: GCRuleToString(fam.GcRule)})
	}
	return ti, nil
}

// SetGCPolicy specifies which cells in a column family should be garbage collected.
// GC executes opportunistically in the background; table reads may return data
// matching the GC policy.
func (ac *AdminClient) SetGCPolicy(ctx context.Context, table, family string, policy GCPolicy) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	req := &btapb.ModifyColumnFamiliesRequest{
		Name: prefix + "/tables/" + table,
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{{
			Id:  family,
			Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Update{&btapb.ColumnFamily{GcRule: policy.proto()}},
		}},
	}
	_, err := ac.tClient.ModifyColumnFamilies(ctx, req)
	return err
}

// DropRowRange permanently deletes a row range from the specified table.
func (ac *AdminClient) DropRowRange(ctx context.Context, table, rowKeyPrefix string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	req := &btapb.DropRowRangeRequest{
		Name:   prefix + "/tables/" + table,
		Target: &btapb.DropRowRangeRequest_RowKeyPrefix{[]byte(rowKeyPrefix)},
	}
	_, err := ac.tClient.DropRowRange(ctx, req)
	return err
}

// CreateTableFromSnapshot creates a table from snapshot.
// The table will be created in the same cluster as the snapshot.
//
// This is a private alpha release of Cloud Bigtable snapshots. This feature
// is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
func (ac *AdminClient) CreateTableFromSnapshot(ctx context.Context, table, cluster, snapshot string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	snapshotPath := prefix + "/clusters/" + cluster + "/snapshots/" + snapshot

	req := &btapb.CreateTableFromSnapshotRequest{
		Parent:         prefix,
		TableId:        table,
		SourceSnapshot: snapshotPath,
	}
	op, err := ac.tClient.CreateTableFromSnapshot(ctx, req)
	if err != nil {
		return err
	}
	resp := btapb.Table{}
	return longrunning.InternalNewOperation(ac.lroClient, op).Wait(ctx, &resp)
}

const DefaultSnapshotDuration time.Duration = 0

// Creates a new snapshot in the specified cluster from the specified source table.
// Setting the ttl to `DefaultSnapshotDuration` will use the server side default for the duration.
//
// This is a private alpha release of Cloud Bigtable snapshots. This feature
// is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
func (ac *AdminClient) SnapshotTable(ctx context.Context, table, cluster, snapshot string, ttl time.Duration) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()

	var ttlProto *durpb.Duration

	if ttl > 0 {
		ttlProto = ptypes.DurationProto(ttl)
	}

	req := &btapb.SnapshotTableRequest{
		Name:       prefix + "/tables/" + table,
		Cluster:    prefix + "/clusters/" + cluster,
		SnapshotId: snapshot,
		Ttl:        ttlProto,
	}

	op, err := ac.tClient.SnapshotTable(ctx, req)
	if err != nil {
		return err
	}
	resp := btapb.Snapshot{}
	return longrunning.InternalNewOperation(ac.lroClient, op).Wait(ctx, &resp)
}

// Returns a SnapshotIterator for iterating over the snapshots in a cluster.
// To list snapshots across all of the clusters in the instance specify "-" as the cluster.
//
// This is a private alpha release of Cloud Bigtable snapshots. This feature
// is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
func (ac *AdminClient) ListSnapshots(ctx context.Context, cluster string) *SnapshotIterator {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	clusterPath := prefix + "/clusters/" + cluster

	it := &SnapshotIterator{}
	req := &btapb.ListSnapshotsRequest{
		Parent: clusterPath,
	}

	fetch := func(pageSize int, pageToken string) (string, error) {
		req.PageToken = pageToken
		if pageSize > math.MaxInt32 {
			req.PageSize = math.MaxInt32
		} else {
			req.PageSize = int32(pageSize)
		}

		resp, err := ac.tClient.ListSnapshots(ctx, req)
		if err != nil {
			return "", err
		}
		for _, s := range resp.Snapshots {
			snapshotInfo, err := newSnapshotInfo(s)
			if err != nil {
				return "", fmt.Errorf("Failed to parse snapshot proto %v", err)
			}
			it.items = append(it.items, snapshotInfo)
		}
		return resp.NextPageToken, nil
	}
	bufLen := func() int { return len(it.items) }
	takeBuf := func() interface{} { b := it.items; it.items = nil; return b }

	it.pageInfo, it.nextFunc = iterator.NewPageInfo(fetch, bufLen, takeBuf)

	return it
}

func newSnapshotInfo(snapshot *btapb.Snapshot) (*SnapshotInfo, error) {
	nameParts := strings.Split(snapshot.Name, "/")
	name := nameParts[len(nameParts)-1]
	tablePathParts := strings.Split(snapshot.SourceTable.Name, "/")
	tableId := tablePathParts[len(tablePathParts)-1]

	createTime, err := ptypes.Timestamp(snapshot.CreateTime)
	if err != nil {
		return nil, fmt.Errorf("Invalid createTime: %v", err)
	}

	deleteTime, err := ptypes.Timestamp(snapshot.DeleteTime)
	if err != nil {
		return nil, fmt.Errorf("Invalid deleteTime: %v", err)
	}

	return &SnapshotInfo{
		Name:        name,
		SourceTable: tableId,
		DataSize:    snapshot.DataSizeBytes,
		CreateTime:  createTime,
		DeleteTime:  deleteTime,
	}, nil
}

// An EntryIterator iterates over log entries.
//
// This is a private alpha release of Cloud Bigtable snapshots. This feature
// is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
type SnapshotIterator struct {
	items    []*SnapshotInfo
	pageInfo *iterator.PageInfo
	nextFunc func() error
}

// PageInfo supports pagination. See https://godoc.org/google.golang.org/api/iterator package for details.
func (it *SnapshotIterator) PageInfo() *iterator.PageInfo {
	return it.pageInfo
}

// Next returns the next result. Its second return value is iterator.Done
// (https://godoc.org/google.golang.org/api/iterator) if there are no more
// results. Once Next returns Done, all subsequent calls will return Done.
func (it *SnapshotIterator) Next() (*SnapshotInfo, error) {
	if err := it.nextFunc(); err != nil {
		return nil, err
	}
	item := it.items[0]
	it.items = it.items[1:]
	return item, nil
}

type SnapshotInfo struct {
	Name        string
	SourceTable string
	DataSize    int64
	CreateTime  time.Time
	DeleteTime  time.Time
}

// Get snapshot metadata.
//
// This is a private alpha release of Cloud Bigtable snapshots. This feature
// is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
func (ac *AdminClient) SnapshotInfo(ctx context.Context, cluster, snapshot string) (*SnapshotInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	clusterPath := prefix + "/clusters/" + cluster
	snapshotPath := clusterPath + "/snapshots/" + snapshot

	req := &btapb.GetSnapshotRequest{
		Name: snapshotPath,
	}

	resp, err := ac.tClient.GetSnapshot(ctx, req)
	if err != nil {
		return nil, err
	}

	return newSnapshotInfo(resp)
}

// Delete a snapshot in a cluster.
//
// This is a private alpha release of Cloud Bigtable snapshots. This feature
// is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
func (ac *AdminClient) DeleteSnapshot(ctx context.Context, cluster, snapshot string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	clusterPath := prefix + "/clusters/" + cluster
	snapshotPath := clusterPath + "/snapshots/" + snapshot

	req := &btapb.DeleteSnapshotRequest{
		Name: snapshotPath,
	}
	_, err := ac.tClient.DeleteSnapshot(ctx, req)
	return err
}

// getConsistencyToken gets the consistency token for a table.
func (ac *AdminClient) getConsistencyToken(ctx context.Context, tableName string) (string, error) {
	req := &btapb.GenerateConsistencyTokenRequest{
		Name: tableName,
	}
	resp, err := ac.tClient.GenerateConsistencyToken(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.GetConsistencyToken(), nil
}

// isConsistent checks if a token is consistent for a table.
func (ac *AdminClient) isConsistent(ctx context.Context, tableName, token string) (bool, error) {
	req := &btapb.CheckConsistencyRequest{
		Name:             tableName,
		ConsistencyToken: token,
	}
	var resp *btapb.CheckConsistencyResponse

	// Retry calls on retryable errors to avoid losing the token gathered before.
	err := gax.Invoke(ctx, func(ctx context.Context) error {
		var err error
		resp, err = ac.tClient.CheckConsistency(ctx, req)
		return err
	}, retryOptions...)
	if err != nil {
		return false, err
	}
	return resp.GetConsistent(), nil
}

// WaitForReplication waits until all the writes committed before the call started have been propagated to all the clusters in the instance via replication.
//
// This is a private alpha release of Cloud Bigtable replication. This feature
// is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
func (ac *AdminClient) WaitForReplication(ctx context.Context, table string) error {
	// Get the token.
	prefix := ac.instancePrefix()
	tableName := prefix + "/tables/" + table
	token, err := ac.getConsistencyToken(ctx, tableName)
	if err != nil {
		return err
	}

	// Periodically check if the token is consistent.
	timer := time.NewTicker(time.Second * 10)
	defer timer.Stop()
	for {
		consistent, err := ac.isConsistent(ctx, tableName, token)
		if err != nil {
			return err
		}
		if consistent {
			return nil
		}
		// Sleep for a bit or until the ctx is cancelled.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

const instanceAdminAddr = "bigtableadmin.googleapis.com:443"

// InstanceAdminClient is a client type for performing admin operations on instances.
// These operations can be substantially more dangerous than those provided by AdminClient.
type InstanceAdminClient struct {
	conn      *grpc.ClientConn
	iClient   btapb.BigtableInstanceAdminClient
	lroClient *lroauto.OperationsClient

	project string

	// Metadata to be sent with each request.
	md metadata.MD
}

// NewInstanceAdminClient creates a new InstanceAdminClient for a given project.
func NewInstanceAdminClient(ctx context.Context, project string, opts ...option.ClientOption) (*InstanceAdminClient, error) {
	o, err := btopt.DefaultClientOptions(instanceAdminAddr, InstanceAdminScope, clientUserAgent)
	if err != nil {
		return nil, err
	}
	o = append(o, opts...)
	conn, err := gtransport.Dial(ctx, o...)
	if err != nil {
		return nil, fmt.Errorf("dialing: %v", err)
	}

	lroClient, err := lroauto.NewOperationsClient(ctx, option.WithGRPCConn(conn))
	if err != nil {
		// This error "should not happen", since we are just reusing old connection
		// and never actually need to dial.
		// If this does happen, we could leak conn. However, we cannot close conn:
		// If the user invoked the function with option.WithGRPCConn,
		// we would close a connection that's still in use.
		// TODO(pongad): investigate error conditions.
		return nil, err
	}

	return &InstanceAdminClient{
		conn:      conn,
		iClient:   btapb.NewBigtableInstanceAdminClient(conn),
		lroClient: lroClient,

		project: project,
		md:      metadata.Pairs(resourcePrefixHeader, "projects/"+project),
	}, nil
}

// Close closes the InstanceAdminClient.
func (iac *InstanceAdminClient) Close() error {
	return iac.conn.Close()
}

// StorageType is the type of storage used for all tables in an instance
type StorageType int

const (
	SSD StorageType = iota
	HDD
)

func (st StorageType) proto() btapb.StorageType {
	if st == HDD {
		return btapb.StorageType_HDD
	}
	return btapb.StorageType_SSD
}

// InstanceType is the type of the instance
type InstanceType int32

const (
	PRODUCTION  InstanceType = InstanceType(btapb.Instance_PRODUCTION)
	DEVELOPMENT              = InstanceType(btapb.Instance_DEVELOPMENT)
)

// InstanceInfo represents information about an instance
type InstanceInfo struct {
	Name        string // name of the instance
	DisplayName string // display name for UIs
}

// InstanceConf contains the information necessary to create an Instance
type InstanceConf struct {
	InstanceId, DisplayName, ClusterId, Zone string
	// NumNodes must not be specified for DEVELOPMENT instance types
	NumNodes     int32
	StorageType  StorageType
	InstanceType InstanceType
}

// InstanceWithClustersConfig contains the information necessary to create an Instance
type InstanceWithClustersConfig struct {
	InstanceID, DisplayName string
	Clusters                []ClusterConfig
	InstanceType            InstanceType
}

var instanceNameRegexp = regexp.MustCompile(`^projects/([^/]+)/instances/([a-z][-a-z0-9]*)$`)

// CreateInstance creates a new instance in the project.
// This method will return when the instance has been created or when an error occurs.
func (iac *InstanceAdminClient) CreateInstance(ctx context.Context, conf *InstanceConf) error {
	newConfig := InstanceWithClustersConfig{
		InstanceID:   conf.InstanceId,
		DisplayName:  conf.DisplayName,
		InstanceType: conf.InstanceType,
		Clusters: []ClusterConfig{
			{
				InstanceID:  conf.InstanceId,
				ClusterID:   conf.ClusterId,
				Zone:        conf.Zone,
				NumNodes:    conf.NumNodes,
				StorageType: conf.StorageType,
			},
		},
	}
	return iac.CreateInstanceWithClusters(ctx, &newConfig)
}

// CreateInstance creates a new instance with configured clusters in the project.
// This method will return when the instance has been created or when an error occurs.
//
// Instances with multiple clusters are part of a private alpha release of Cloud Bigtable replication.
// This feature is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
func (iac *InstanceAdminClient) CreateInstanceWithClusters(ctx context.Context, conf *InstanceWithClustersConfig) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	clusters := make(map[string]*btapb.Cluster)
	for _, cluster := range conf.Clusters {
		clusters[cluster.ClusterID] = cluster.proto(iac.project)
	}

	req := &btapb.CreateInstanceRequest{
		Parent:     "projects/" + iac.project,
		InstanceId: conf.InstanceID,
		Instance:   &btapb.Instance{DisplayName: conf.DisplayName, Type: btapb.Instance_Type(conf.InstanceType)},
		Clusters:   clusters,
	}

	lro, err := iac.iClient.CreateInstance(ctx, req)
	if err != nil {
		return err
	}
	resp := btapb.Instance{}
	return longrunning.InternalNewOperation(iac.lroClient, lro).Wait(ctx, &resp)
}

// DeleteInstance deletes an instance from the project.
func (iac *InstanceAdminClient) DeleteInstance(ctx context.Context, instanceId string) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.DeleteInstanceRequest{"projects/" + iac.project + "/instances/" + instanceId}
	_, err := iac.iClient.DeleteInstance(ctx, req)
	return err
}

// Instances returns a list of instances in the project.
func (iac *InstanceAdminClient) Instances(ctx context.Context) ([]*InstanceInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.ListInstancesRequest{
		Parent: "projects/" + iac.project,
	}
	res, err := iac.iClient.ListInstances(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(res.FailedLocations) > 0 {
		// We don't have a good way to return a partial result in the face of some zones being unavailable.
		// Fail the entire request.
		return nil, status.Errorf(codes.Unavailable, "Failed locations: %v", res.FailedLocations)
	}

	var is []*InstanceInfo
	for _, i := range res.Instances {
		m := instanceNameRegexp.FindStringSubmatch(i.Name)
		if m == nil {
			return nil, fmt.Errorf("malformed instance name %q", i.Name)
		}
		is = append(is, &InstanceInfo{
			Name:        m[2],
			DisplayName: i.DisplayName,
		})
	}
	return is, nil
}

// InstanceInfo returns information about an instance.
func (iac *InstanceAdminClient) InstanceInfo(ctx context.Context, instanceId string) (*InstanceInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.GetInstanceRequest{
		Name: "projects/" + iac.project + "/instances/" + instanceId,
	}
	res, err := iac.iClient.GetInstance(ctx, req)
	if err != nil {
		return nil, err
	}

	m := instanceNameRegexp.FindStringSubmatch(res.Name)
	if m == nil {
		return nil, fmt.Errorf("malformed instance name %q", res.Name)
	}
	return &InstanceInfo{
		Name:        m[2],
		DisplayName: res.DisplayName,
	}, nil
}

// ClusterConfig contains the information necessary to create a cluster
type ClusterConfig struct {
	InstanceID, ClusterID, Zone string
	NumNodes                    int32
	StorageType                 StorageType
}

func (cc *ClusterConfig) proto(project string) *btapb.Cluster {
	return &btapb.Cluster{
		ServeNodes:         cc.NumNodes,
		DefaultStorageType: cc.StorageType.proto(),
		Location:           "projects/" + project + "/locations/" + cc.Zone,
	}
}

// ClusterInfo represents information about a cluster.
type ClusterInfo struct {
	Name       string // name of the cluster
	Zone       string // GCP zone of the cluster (e.g. "us-central1-a")
	ServeNodes int    // number of allocated serve nodes
	State      string // state of the cluster
}

// CreateCluster creates a new cluster in an instance.
// This method will return when the cluster has been created or when an error occurs.
//
// This is a private alpha release of Cloud Bigtable replication. This feature
// is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
func (iac *InstanceAdminClient) CreateCluster(ctx context.Context, conf *ClusterConfig) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)

	req := &btapb.CreateClusterRequest{
		Parent:    "projects/" + iac.project + "/instances/" + conf.InstanceID,
		ClusterId: conf.ClusterID,
		Cluster:   conf.proto(iac.project),
	}

	lro, err := iac.iClient.CreateCluster(ctx, req)
	if err != nil {
		return err
	}
	resp := btapb.Cluster{}
	return longrunning.InternalNewOperation(iac.lroClient, lro).Wait(ctx, &resp)
}

// DeleteCluster deletes a cluster from an instance.
//
// This is a private alpha release of Cloud Bigtable replication. This feature
// is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
func (iac *InstanceAdminClient) DeleteCluster(ctx context.Context, instanceId, clusterId string) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.DeleteClusterRequest{"projects/" + iac.project + "/instances/" + instanceId + "/clusters/" + clusterId}
	_, err := iac.iClient.DeleteCluster(ctx, req)
	return err
}

// UpdateCluster updates attributes of a cluster
func (iac *InstanceAdminClient) UpdateCluster(ctx context.Context, instanceId, clusterId string, serveNodes int32) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	cluster := &btapb.Cluster{
		Name:       "projects/" + iac.project + "/instances/" + instanceId + "/clusters/" + clusterId,
		ServeNodes: serveNodes}
	lro, err := iac.iClient.UpdateCluster(ctx, cluster)
	if err != nil {
		return err
	}
	return longrunning.InternalNewOperation(iac.lroClient, lro).Wait(ctx, nil)
}

// Clusters lists the clusters in an instance.
func (iac *InstanceAdminClient) Clusters(ctx context.Context, instanceId string) ([]*ClusterInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.ListClustersRequest{Parent: "projects/" + iac.project + "/instances/" + instanceId}
	res, err := iac.iClient.ListClusters(ctx, req)
	if err != nil {
		return nil, err
	}
	// TODO(garyelliott): Deal with failed_locations.
	var cis []*ClusterInfo
	for _, c := range res.Clusters {
		nameParts := strings.Split(c.Name, "/")
		locParts := strings.Split(c.Location, "/")
		cis = append(cis, &ClusterInfo{
			Name:       nameParts[len(nameParts)-1],
			Zone:       locParts[len(locParts)-1],
			ServeNodes: int(c.ServeNodes),
			State:      c.State.String(),
		})
	}
	return cis, nil
}

// GetCluster fetches a cluster in an instance
func (iac *InstanceAdminClient) GetCluster(ctx context.Context, instanceID, clusterID string) (*ClusterInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.GetClusterRequest{Name: "projects/" + iac.project + "/instances/" + instanceID + "/clusters" + clusterID}
	c, err := iac.iClient.GetCluster(ctx, req)
	if err != nil {
		return nil, err
	}

	nameParts := strings.Split(c.Name, "/")
	locParts := strings.Split(c.Location, "/")
	cis := &ClusterInfo{
		Name:       nameParts[len(nameParts)-1],
		Zone:       locParts[len(locParts)-1],
		ServeNodes: int(c.ServeNodes),
		State:      c.State.String(),
	}
	return cis, nil
}
