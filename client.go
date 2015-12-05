package hbase

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	pb "github.com/golang/protobuf/proto"
	"github.com/juju/errors"
	"github.com/ngaut/go-zookeeper/zk"
	"github.com/ngaut/log"
	"github.com/pingcap/go-hbase/proto"
)

const (
	zkRootRegionPath = "/meta-region-server"
	zkMasterAddrPath = "/master"

	magicHeadByte           = 0xff
	magicHeadSize           = 1
	idLengthSize            = 4
	md5HexSize              = 32
	servernameSeparator     = ","
	rpcTimeout              = 30000
	pingTimeout             = 30000
	callTimeout             = 5000
	defaultMaxActionRetries = 3
	// Some operations can take a long time such as disable of big table.
	// numRetries is for 'normal' stuff... Multiply by this factor when
	// want to wait a long time.
	retryLongerMultiplier    = 31
	socketDefaultRetryWaitMs = 200
	defaultRetryWaitMs       = 100
	// always >= any unix timestamp(hbase version)
	beyondMaxTimestamp = "99999999999999"
)

var (
	hbaseHeaderBytes []byte = []byte("HBas")
	metaTableName    []byte = []byte("hbase:meta")
	metaRegionName   []byte = []byte("hbase:meta,,1")
)

var retryPauseTime = []int64{1, 2, 3, 5, 10, 20, 40, 100, 100, 100, 100, 200, 200}

type RegionInfo struct {
	Server         string
	StartKey       []byte
	EndKey         []byte
	Name           string
	Ts             string
	TableNamespace string
	TableName      string
	Offline        bool
	Split          bool
}

type tableInfo struct {
	tableName string
	families  []string
}

// export client interface
type HBaseClient interface {
	Get(tbl string, g *Get) (*ResultRow, error)
	Put(tbl string, p *Put) (bool, error)
	Delete(tbl string, d *Delete) (bool, error)
	TableExists(tbl string) (bool, error)
	DropTable(t TableName) error
	DisableTable(t TableName) error
	CreateTable(t *TableDescriptor, splits [][]byte) error
	ServiceCall(table string, call *CoprocessorServiceCall) (*proto.CoprocessorServiceResponse, error)
	LocateRegion(table, row []byte, useCache bool) (*RegionInfo, error)
	CleanRegionCache(table []byte)
	CleanAllRegionCache()
	Close() error
}

// hbase client implemetation
var _ HBaseClient = (*client)(nil)

type client struct {
	mu               sync.RWMutex // for read/update region info
	zkClient         *zk.Conn
	zkHosts          []string
	zkRoot           string
	prefetched       map[string]bool
	cachedConns      map[string]*connection
	cachedRegionInfo map[string]map[string]*RegionInfo
	maxRetries       int
	rootServerName   *proto.ServerName
	masterServerName *proto.ServerName
}

func serverNameToAddr(server *proto.ServerName) string {
	return fmt.Sprintf("%s:%d", server.GetHostName(), server.GetPort())
}

func NewClient(zkHosts []string, zkRoot string) (HBaseClient, error) {
	cl := &client{
		zkHosts:          zkHosts,
		zkRoot:           zkRoot,
		cachedConns:      make(map[string]*connection),
		cachedRegionInfo: make(map[string]map[string]*RegionInfo),
		prefetched:       make(map[string]bool),
		maxRetries:       defaultMaxActionRetries,
	}

	err := cl.init()
	if err != nil {
		return nil, errors.Trace(err)
	}

	return cl, nil
}

func (c *client) decodeMeta(data []byte) (*proto.ServerName, error) {
	if data[0] != magicHeadByte {
		return nil, errors.New("unknown packet")
	}

	var n int32
	err := binary.Read(bytes.NewBuffer(data[1:]), binary.BigEndian, &n)
	if err != nil {
		return nil, errors.Trace(err)
	}

	dataOffset := magicHeadSize + idLengthSize + int(n)
	data = data[(dataOffset + 4):]

	var mrs proto.MetaRegionServer
	err = pb.Unmarshal(data, &mrs)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return mrs.GetServer(), nil
}

// init and get root region server addr and master addr
func (c *client) init() error {
	zkclient, _, err := zk.Connect(c.zkHosts, time.Second*30)
	if err != nil {
		return errors.Trace(err)
	}
	c.zkClient = zkclient

	res, _, _, err := c.zkClient.GetW(c.zkRoot + zkRootRegionPath)
	if err != nil {
		return errors.Trace(err)
	}

	c.rootServerName, err = c.decodeMeta(res)
	if err != nil {
		return errors.Trace(err)
	}

	log.Debug("connect root region server...", c.rootServerName)
	conn, err := newConnection(serverNameToAddr(c.rootServerName), false)
	if err != nil {
		return errors.Trace(err)
	}

	// set buffered regionserver conn
	c.cachedConns[serverNameToAddr(c.rootServerName)] = conn

	res, _, _, err = c.zkClient.GetW(c.zkRoot + zkMasterAddrPath)
	if err != nil {
		return errors.Trace(err)
	}

	c.masterServerName, err = c.decodeMeta(res)
	if err != nil {
		return errors.Trace(err)
	}

	return nil
}

// get connection
func (c *client) getConn(addr string, isMaster bool) (*connection, error) {
	c.mu.RLock()
	conn, ok := c.cachedConns[addr]
	c.mu.RUnlock()

	if ok {
		return conn, nil
	}

	var err error
	conn, err = newConnection(addr, isMaster)
	if err != nil {
		return nil, errors.Errorf("create new connection failed - %v", errors.ErrorStack(err))
	}

	c.mu.Lock()
	c.cachedConns[addr] = conn
	c.mu.Unlock()

	return conn, nil
}

func (c *client) getRegionConn(addr string) (*connection, error) {
	return c.getConn(addr, false)
}

func (c *client) getMasterConn() (*connection, error) {
	return c.getConn(serverNameToAddr(c.masterServerName), true)
}

func (c *client) adminAction(req pb.Message) (chan pb.Message, error) {
	conn, err := c.getMasterConn()
	if err != nil {
		return nil, errors.Trace(err)
	}

	cl := newCall(req)
	err = conn.call(cl)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return cl.responseCh, nil
}

// http://stackoverflow.com/questions/27602013/correct-way-to-get-region-name-by-using-hbase-api
func (c *client) createRegionName(table, startKey []byte, id string, newFormat bool) []byte {
	if len(startKey) == 0 {
		startKey = make([]byte, 1)
	}

	b := bytes.Join([][]byte{table, startKey, []byte(id)}, []byte{','})

	if newFormat {
		m := md5.Sum(b)
		mhex := []byte(hex.EncodeToString(m[:]))
		b = append(bytes.Join([][]byte{b, mhex}, []byte{'.'}), '.')
	}

	return b
}

func (c *client) parseRegion(rr *ResultRow) (*RegionInfo, error) {
	regionInfoCol, ok := rr.Columns["info:regioninfo"]
	if !ok {
		return nil, errors.Errorf("Unable to parse region location (no regioninfo column): %#v", rr)
	}

	offset := bytes.Index(regionInfoCol.Value, []byte("PBUF")) + 4
	regionInfoBytes := regionInfoCol.Value[offset:]

	var info proto.RegionInfo
	err := pb.Unmarshal(regionInfoBytes, &info)
	if err != nil {
		return nil, errors.Errorf("Unable to parse region location: %#v", err)
	}

	ri := &RegionInfo{
		StartKey:       info.GetStartKey(),
		EndKey:         info.GetEndKey(),
		Name:           bytes.NewBuffer(rr.Row).String(),
		TableNamespace: string(info.GetTableName().GetNamespace()),
		TableName:      string(info.GetTableName().GetQualifier()),
		Offline:        info.GetOffline(),
		Split:          info.GetSplit(),
	}

	if v, ok := rr.Columns["info:server"]; ok {
		ri.Server = string(v.Value)
	}

	return ri, nil
}

func (c *client) getMetaRegion() *RegionInfo {
	return &RegionInfo{
		StartKey: []byte{},
		EndKey:   []byte{},
		Name:     string(metaRegionName),
		Server:   serverNameToAddr(c.rootServerName),
	}
}

func (c *client) getCachedLocation(table, row []byte) *RegionInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()

	tableStr := string(table)
	if regions, ok := c.cachedRegionInfo[tableStr]; ok {
		for _, region := range regions {
			if (len(region.EndKey) == 0 ||
				bytes.Compare(row, region.EndKey) < 0) &&
				(len(region.StartKey) == 0 ||
					bytes.Compare(row, region.StartKey) >= 0) {
				return region
			}
		}
	}

	return nil
}

func (c *client) updateRegionCache(table []byte, region *RegionInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()

	tableStr := string(table)
	if _, ok := c.cachedRegionInfo[tableStr]; !ok {
		c.cachedRegionInfo[tableStr] = make(map[string]*RegionInfo)
	}
	c.cachedRegionInfo[tableStr][region.Name] = region
}

func (c *client) CleanRegionCache(table []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cachedRegionInfo, string(table))
}

func (c *client) CleanAllRegionCache() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cachedRegionInfo = map[string]map[string]*RegionInfo{}
}

func (c *client) LocateRegion(table, row []byte, useCache bool) (*RegionInfo, error) {
	// If user wants to locate metaregion, just return it.
	if bytes.Equal(table, metaTableName) {
		return c.getMetaRegion(), nil
	}

	// Try to get location from cache.
	if useCache {
		if r := c.getCachedLocation(table, row); r != nil {
			return r, nil
		}
	}

	// If cache missed or not using cache, try to get and update region info.
	metaRegion := c.getMetaRegion()
	conn, err := c.getRegionConn(metaRegion.Server)
	if err != nil {
		return nil, errors.Trace(err)
	}

	regionRow := c.createRegionName(table, row, beyondMaxTimestamp, true)

	call := newCall(&proto.GetRequest{
		Region: &proto.RegionSpecifier{
			Type:  proto.RegionSpecifier_REGION_NAME.Enum(),
			Value: metaRegionName,
		},
		Get: &proto.Get{
			Row: regionRow,
			Column: []*proto.Column{&proto.Column{
				Family: []byte("info"),
			}},
			ClosestRowBefore: pb.Bool(true),
		},
	})

	err = conn.call(call)
	if err != nil {
		return nil, errors.Trace(err)
	}

	response := <-call.responseCh

	switch r := response.(type) {
	case *proto.GetResponse:
		res := r.GetResult()
		if res == nil {
			return nil, errors.Errorf("Empty region: [table=%s] [row=%q] [region_row=%q]", table, row, regionRow)
		}

		rr := NewResultRow(res)
		region, err := c.parseRegion(rr)
		if err != nil {
			return nil, errors.Trace(err)
		}

		c.updateRegionCache(table, region)
		return region, nil
	case *exception:
		return nil, errors.New(r.msg)
	default:
		log.Warnf("Unknown response - %T - %v", r, r)
	}

	return nil, errors.Errorf("Couldn't find the region: [table=%s] [row=%q] [region_row=%q]", table, row, regionRow)
}

func (c *client) Close() error {
	if c.zkClient != nil {
		c.zkClient.Close()
	}

	for _, conn := range c.cachedConns {
		err := conn.close()
		if err != nil {
			return errors.Trace(err)
		}
	}

	return nil
}
