package hbase

import (
	"fmt"

	pb "github.com/golang/protobuf/proto"
	. "github.com/pingcap/check"
	"github.com/pingcap/go-hbase/proto"
)

type mockAction struct {
}

func (m *mockAction) ToProto() pb.Message {
	return &mockMessage{}
}

type mockMessage struct {
}

func (m *mockMessage) Reset() {
}

func (m *mockMessage) String() string {
	return "mock message"
}

func (m *mockMessage) ProtoMessage() {
}

type Message interface {
	Reset()
	String() string
	ProtoMessage()
}

type ActionTestSuit struct {
	cli       HBaseClient
	tableName string
}

var _ = Suite(&ActionTestSuit{})

func (s *ActionTestSuit) SetUpTest(c *C) {
	var err error
	s.cli, err = NewClient(getTestZkHosts(), "/hbase")
	c.Assert(err, IsNil)

	s.tableName = "test_action"
	tblDesc := NewTableDesciptor(NewTableNameWithDefaultNS(s.tableName))
	cf := NewColumnFamilyDescriptor("cf")
	tblDesc.AddColumnDesc(cf)
	err = s.cli.CreateTable(tblDesc, nil)
	c.Assert(err, IsNil)
}

func (s *ActionTestSuit) TearDownTest(c *C) {
	err := s.cli.DisableTable(NewTableNameWithDefaultNS(s.tableName))
	c.Assert(err, IsNil)

	err = s.cli.DropTable(NewTableNameWithDefaultNS(s.tableName))
	c.Assert(err, IsNil)

	fmt.Println("[drop table]", s.tableName)
}

func (s *ActionTestSuit) TestDo(c *C) {
	client, ok := s.cli.(*client)
	c.Assert(ok, IsTrue)

	row := []byte("row1")
	value := []byte("value1")
	p := NewPut(row)
	p.AddValue([]byte("cf"), []byte("q"), value)

	// Test put action.
	msg, err := client.do([]byte(s.tableName), row, p, true)
	c.Assert(err, IsNil)

	res, ok := msg.(*proto.MutateResponse)
	c.Assert(ok, IsTrue)
	c.Assert(res.GetProcessed(), IsTrue)
	// cachedConns includes master conn and region conn.
	c.Assert(client.cachedConns, HasLen, 2)
	// cachedRegionInfo includes table to regions mapping.
	c.Assert(client.cachedRegionInfo, HasLen, 1)

	// Test not use cache conn and check cachedConns.
	msg, err = client.do([]byte(s.tableName), row, p, false)
	c.Assert(err, IsNil)

	res, ok = msg.(*proto.MutateResponse)
	c.Assert(ok, IsTrue)
	c.Assert(res.GetProcessed(), IsTrue)
	c.Assert(client.cachedConns, HasLen, 2)
	c.Assert(client.cachedRegionInfo, HasLen, 1)

	// Test use cache conn and check cachedConns.
	msg, err = client.do([]byte(s.tableName), row, p, true)
	c.Assert(err, IsNil)

	res, ok = msg.(*proto.MutateResponse)
	c.Assert(ok, IsTrue)
	c.Assert(res.GetProcessed(), IsTrue)
	c.Assert(client.cachedConns, HasLen, 2)
	c.Assert(client.cachedRegionInfo, HasLen, 1)

	// Test put value to a none-exist table.
	_, err = client.do([]byte("unknown-table"), row, p, true)
	c.Assert(err, NotNil)
	c.Assert(client.cachedConns, HasLen, 2)
	c.Assert(client.cachedRegionInfo, HasLen, 1)

	// Test get action.
	g := NewGet(row)
	g.AddColumn([]byte("cf"), []byte("q"))

	msg, err = client.do([]byte(s.tableName), row, g, true)
	c.Assert(err, IsNil)

	gres, ok := msg.(*proto.GetResponse)
	c.Assert(ok, IsTrue)
	rr := NewResultRow(gres.GetResult())
	c.Assert(rr, NotNil)
	c.Assert(rr.SortedColumns[0].Value, DeepEquals, value)
	c.Assert(client.cachedConns, HasLen, 2)
	c.Assert(client.cachedRegionInfo, HasLen, 1)

	// Test delete action.
	d := NewDelete(row)
	d.AddFamily([]byte("cf"))

	msg, err = client.innerDo([]byte(s.tableName), row, d, true)
	c.Assert(err, IsNil)

	res, ok = msg.(*proto.MutateResponse)
	c.Assert(ok, IsTrue)
	c.Assert(res.GetProcessed(), IsTrue)
	c.Assert(client.cachedConns, HasLen, 2)
	c.Assert(client.cachedRegionInfo, HasLen, 1)

	// TODO: Test CoprocessorServiceCall.

	// Test error.
	m := &mockAction{}

	msg, err = client.innerDo([]byte(s.tableName), row, m, true)
	c.Assert(err, NotNil)
	c.Assert(client.cachedConns, HasLen, 2)
	c.Assert(client.cachedRegionInfo, HasLen, 1)
}
