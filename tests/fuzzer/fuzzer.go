package main

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"github.com/jackc/pgconn"
	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgproto3/v2"
	"github.com/jackc/pgx/v4"
	proto "github.com/golang/protobuf/proto"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	SQL_INTEGER = iota
	SQL_BYTEA
	//SQL_TEXT
)

type SQLValue struct {
	Null bool
	Binary bool
	Datum []byte
}

type SQLType int

func (t SQLType) String() string {
	switch t {
		case SQL_INTEGER:
			return "int4"
		case SQL_BYTEA:
			return "bytea"
		default:
			panic(t)
	}
}

func (t SQLType) Oid() uint32 {
	switch t {
		case SQL_INTEGER:
			return 23
		case SQL_BYTEA:
			return 17
		default:
			panic(t)
	}
}

type FuzzerError struct {
	Transaction *TestTransaction
	ExpectedMessages []proto.Message
	ReceivedMessages []proto.Message
	Err error
}

func (fe *FuzzerError) DescribeExpectedMessages() string {
	var descriptions []string
	for _, msg := range fe.ExpectedMessages {
		desc := fmt.Sprintf("%T%s", msg, proto.MarshalTextString(msg))
		descriptions = append(descriptions, desc)
	}
	return strings.Join(descriptions, "\n")
}

func (fe *FuzzerError) DescribeReceivedMessages() string {
	var descriptions []string
	for _, msg := range fe.ReceivedMessages {
		desc := fmt.Sprintf("%T%s", msg, proto.MarshalTextString(msg))
		descriptions = append(descriptions, desc)
	}
	return strings.Join(descriptions, "\n")
}

func (fe *FuzzerError) Error() string {
	return fe.Err.Error()
}

type TestSchema struct {
	TableName string
	NumColumns int
	ColumnNames []string
	ColumnTypes []SQLType
}

type TestCase struct {
}

var replicationSlotName string = "pgpb3ldtest"
var outputPluginName string = "pg_pb3_ld"

func generateSQLType() SQLType {
	v := rand.Intn(1)
	if v == 0 {
		return SQL_INTEGER
	} else {
		panic(v)
	}
}

type SchemaGenerator interface {
	GenerateSchema() *TestSchema
}

type TransactionGenerator interface {
	GenerateTransaction() *TestTransaction
}

type TestTransaction struct {
	Operations []TestOperation
}

func (t *TestTransaction) Describe() string {
	var descriptions []string
	for _, op := range t.Operations {
		descriptions = append(descriptions, op.Describe())
	}
	return strings.Join(descriptions, "\n")
}

type TestOperation interface {
	Execute(schema *TestSchema, txn pgx.Tx) error
	ExpectedMessages(schema *TestSchema) []proto.Message
	Describe() string
}

type TestInsert struct {
	TableName string
	Values []SQLValue
}

func (ti *TestInsert) Execute(schema *TestSchema, txn pgx.Tx) error {
	conn := txn.Conn().PgConn()

	sql := "INSERT INTO \"" + ti.TableName + "\" VALUES ("
	paramFormats := make([]int16, len(ti.Values))
	paramValues := make([][]byte, len(ti.Values))
	paramOids := make([]uint32, len(ti.Values))
	for i, val := range ti.Values {
		if val.Null {
			paramValues[i] = nil
		} else {
			paramValues[i] = val.Datum
		}
		if val.Binary {
			paramFormats[i] = 1
		} else {
			paramFormats[i] = 0
		}
		paramOids[i] = schema.ColumnTypes[i].Oid()
	}
	for i := range ti.Values {
		if i > 0 {
			sql += ", "
		}
		sql += "$" + strconv.Itoa(i + 1)
	}
	sql += ")"

	if len(ti.Values) == 0 {
		sql = "INSERT INTO \"" + ti.TableName + "\" DEFAULT VALUES"
	}

	res := conn.ExecParams(context.Background(), sql, paramValues, paramOids, paramFormats, nil)
	_, err := res.Close()
	if err != nil {
		return err
	}

	return nil
}

func (ti *TestInsert) ExpectedMessages(schema *TestSchema) []proto.Message {
	values := make([][]byte, len(ti.Values))
	// omit_nulls
	var typeOids []uint32
	nulls := make([]byte, len(ti.Values))
	// disabled
	formats := []byte(nil)
	for i, val := range ti.Values {
		if val.Null {
			values[i] = []byte{}
			nulls[i] = '\x01'
		} else {
			values[i] = val.Datum
			nulls[i] = '\x00'
			typeOids = append(typeOids, schema.ColumnTypes[i].Oid())
		}
	}
	id := &InsertDescription{
		Table: &TableDescription{
			SchemaName: "public",
			TableName: ti.TableName,
		},
		NewValues: &FieldSetDescription{
			Names: schema.ColumnNames,
			Values: values,
			TypeOids: typeOids,
			Nulls: nulls,
			Formats: formats,
		},
	}
	return []proto.Message{id}
}

func (ti *TestInsert) Describe() string {
	value := "Insert " + ti.TableName + " {\n"
	for i, val := range ti.Values {
		if i > 0 {
			value += ",\n"
		}
		if val.Null {
			value += "    nil"
		} else {
			value += fmt.Sprintf("    %q", val.Datum)
		}
	}
	value += "\n}"
	return value
}

func generateSQLValue(t SQLType) []byte {
	if t == SQL_INTEGER {
		r := rand.Int63n(4294967296)
		return []byte(strconv.Itoa(int(r - 2147483648)))
	} else {
		panic(t)
	}
}

func generateSQLIdentifier() string {
	alphabet := []byte("abcdefghijklmnopqrstuvwxyz_ABCDEFGHIJKLMNOPQRSTUVWXYZ")

	var length int
	for {
		length = int(rand.NormFloat64() * 10 + 5)
		if length > 0 && length <= 63 {
			break
		}
	}
	value := ""
	for i := 0; i < length; i++ {
		value += string(alphabet[rand.Intn(len(alphabet))])
	}
	return value
}

func generateFuzzedSchema() TestSchema {
	schema := TestSchema{}

	for {
		schema.NumColumns = int(rand.NormFloat64() * 10 + 5)
		if schema.NumColumns > 0 && schema.NumColumns < 512 {
			break
		}
	}

	schema.TableName = generateSQLIdentifier()
	schema.ColumnNames = make([]string, schema.NumColumns)
	schema.ColumnTypes = make([]SQLType, schema.NumColumns)
	columns := make(map[string]struct{})
	for i := 0; i < schema.NumColumns; i++ {
		for {
			schema.ColumnNames[i] = generateSQLIdentifier()
			_, exists := columns[schema.ColumnNames[i]]
			if !exists {
				columns[schema.ColumnNames[i]] = struct{}{}
				break
			}
		}
		schema.ColumnTypes[i] = generateSQLType()
	}

	return schema
}

func (s *TestSchema) SetupSQL() string {
	sql := ""
	sql += fmt.Sprintf(`DROP TABLE IF EXISTS "%s";`, s.TableName)
	sql += "\n\n"
	sql += fmt.Sprintf(`CREATE TABLE "%s" (`, s.TableName)
	sql += "\n"
	for i, colname := range s.ColumnNames {
		coltype := s.ColumnTypes[i]
		if i > 0 {
			sql += ",\n"
		}
		sql += fmt.Sprintf(`    "%s" %s`, colname, coltype.String())
	}
	sql += "\n);"
	return sql
}

func (s *TestSchema) TeardownSQL() string {
	return fmt.Sprintf(`DROP TABLE IF EXISTS "%s";`, s.TableName)
}

type DecodedMessage struct {
	LSN pglogrepl.LSN

	// Only one of the following fields will be set.
	Err error
	Message proto.Message
}

type Fuzzer struct {
	dbh *pgx.Conn
	conninfo []string

	replConn *pgconn.PgConn
	replCancel context.CancelFunc
	replMessageChan chan *DecodedMessage
}

func NewFuzzer(conninfo []string) *Fuzzer {
	dbh, err := pgx.Connect(context.Background(), strings.Join(conninfo, " "))
	if err != nil {
		log.Fatal(err)
	}
	var isSuperUser string
	err = dbh.QueryRow(context.Background(), "SHOW is_superuser").Scan(&isSuperUser)
	if err != nil {
		panic(err)
	}
	if isSuperUser != "on" {
		panic(fmt.Sprintf("not a superuser (got %q; expected \"on\")", isSuperUser))
	}

	fuzzer := &Fuzzer{
		dbh: dbh,
		replConn: nil,
		conninfo: conninfo,
	}

	fuzzer.createReplicationSlot()

	return fuzzer
}

func (f *Fuzzer) createReplicationSlot() {
	_, err := f.dbh.Exec(context.Background(), `SELECT pg_create_logical_replication_slot($1, $2)`, replicationSlotName, outputPluginName)
	if err != nil {
		pge, ok := err.(*pgconn.PgError)
		if !ok {
			panic(err)
		}
		if pge.Code != "42710" {
			panic(err)
		}
		_, err = f.dbh.Exec(context.Background(), "SELECT pg_drop_replication_slot($1)", replicationSlotName)
		if err != nil {
			panic(err)
		}
		_, err = f.dbh.Exec(context.Background(), `SELECT pg_create_logical_replication_slot($1, $2)`, replicationSlotName, outputPluginName)
		if err != nil {
			panic(err)
		}
	}
}

func (f *Fuzzer) openReplicationConnection() {
	if f.replConn != nil {
		panic("uh oh")
	}

	options := []string{
		"type_oids_mode 'omit_nulls'",
		"formats_mode 'disabled'",
		"binary_oid_ranges '1-200000'",
	}

	replConnInfo := append(f.conninfo, "replication=database")
	replConn, err := pgconn.Connect(context.Background(), strings.Join(replConnInfo, " "))
	if err != nil {
		panic(err)
	}
	sysident, err := pglogrepl.IdentifySystem(context.Background(), replConn)
	if err != nil {
		panic(err)
	}
	err = pglogrepl.StartReplication(
		context.Background(),
		replConn,
		replicationSlotName,
		sysident.XLogPos,
		pglogrepl.StartReplicationOptions{
			PluginArgs: options,
		},
	)
	if err != nil {
		panic(err)
	}
	f.replConn = replConn

	ctx, cancel := context.WithCancel(context.Background())
	f.replCancel = cancel
	f.replMessageChan = make(chan *DecodedMessage, 1)
	go f.backgroundReceiveLogicalDecodingMessages(ctx)
}

func (f *Fuzzer) closeReplicationConnection() {
	if f.replConn != nil {
		f.shutdownLogicalReceiver()
		_ = f.replConn.Close(context.Background())
		f.replConn = nil
		f.replCancel()
		f.replCancel = nil
		f.replMessageChan = nil
	}
}

func (f *Fuzzer) MainLoop() {
	sg := NewExhaustiveSchemaGenerator()
	for {
		schema := sg.GenerateSchema()
		generator := NewExhaustiveTransactionGenerator(schema)
		err := f.testMain(schema, generator)
		if err != nil {
			f.closeReplicationConnection()
			time.Sleep(5 * time.Second)
		}
		//time.Sleep(time.Second)
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *Fuzzer) testMain(schema *TestSchema, generator TransactionGenerator) error {
	sql := schema.SetupSQL()
	defer func() {
		_, _ = f.dbh.Exec(context.Background(), schema.TeardownSQL())
	}()
	err := testSetup(f.dbh, sql)
	if err != nil {
		f.logFuzzError("setup", err, sql)
		return err
	}

	if f.replConn == nil {
		f.openReplicationConnection()
	}

	err = f.runTests(schema, generator)
	if err != nil {
		f.logFuzzError("run", err, sql)
		return err
	}

	return nil
}

func (f *Fuzzer) runTests(schema *TestSchema, generator TransactionGenerator) error {
	var minimumLSN pglogrepl.LSN
	err := f.dbh.QueryRow(context.Background(), "SELECT pg_current_xlog_location()").Scan(&minimumLSN)
	if err != nil {
		panic(err)
	}

	for {
		txn := generator.GenerateTransaction()
		if txn == nil {
			break
		}

		dbtxn, err := f.dbh.Begin(context.Background())
		if err != nil {
			return err
		}

		var expectedMessages []proto.Message
		for _, op := range txn.Operations {
			err := op.Execute(schema, dbtxn)
			if err != nil {
				_ = dbtxn.Rollback(context.Background())
				return err
			}
			expectedMessages = append(expectedMessages, op.ExpectedMessages(schema)...)
		}
		expectedMessages = append(expectedMessages, &CommitTransaction{})

		err = dbtxn.Commit(context.Background())
		if err != nil {
			return err
		}

		var receivedMessages []proto.Message
		for _, expectedMessage := range expectedMessages {
			var decodedMessage *DecodedMessage
			select {
				case decodedMessage = <-f.replMessageChan:
				case <-time.After(15 * time.Second):
					return &FuzzerError{
						Transaction: txn,
						ExpectedMessages: expectedMessages,
						ReceivedMessages: receivedMessages,
						Err: fmt.Errorf("timed out while waiting for DecodedMessage"),
					}
			}
			if decodedMessage.Err != nil {
				return decodedMessage.Err
			}
			if decodedMessage.LSN < minimumLSN {
				continue
			}
			msg := decodedMessage.Message
			receivedMessages = append(receivedMessages, msg)
			if !proto.Equal(msg, expectedMessage) {
				return &FuzzerError{
					Transaction: txn,
					ExpectedMessages: expectedMessages,
					ReceivedMessages: receivedMessages,
					Err: fmt.Errorf(
						"message does not match:\n    %T:%+v\n\n  is not equal to\n\n    %T:%+v",
						msg, msg, expectedMessage, expectedMessage,
					),
				}
			}
		}
	}

	return nil
}

func (f *Fuzzer) shutdownLogicalReceiver() {
	f.replCancel()
	for {
		msg, ok := <-f.replMessageChan
		if !ok {
			panic(ok)
		}

		if msg == nil {
			break
		}
	}

	// At this point the channel should be closed.  Make sure it is, and then
	// clean up.
	select {
		case _, ok := <-f.replMessageChan:
			if ok {
				panic(ok)
			}
		case <-time.After(time.Second):
			panic("timeout")
	}
}

func (f *Fuzzer) backgroundReceiveLogicalDecodingMessages(ctx context.Context) {
	sendStatusUpdate := false
	clientLSN, err := pglogrepl.ParseLSN("0/0")
	if err != nil {
		panic(err)
	}
	for {
		if sendStatusUpdate {
			// We intentionally don't use "ctx" here, since this should be a
			// really short call.
			commDeadline := time.Now().Add(5 * time.Second)
			commCtx, cancel := context.WithDeadline(context.Background(), commDeadline)
			err := pglogrepl.SendStandbyStatusUpdate(
				commCtx,
				f.replConn,
				pglogrepl.StandbyStatusUpdate{
					WALWritePosition: clientLSN,
					WALFlushPosition: clientLSN,
					WALApplyPosition: clientLSN,
					ClientTime: time.Now(),
					ReplyRequested: false,
				},
			)
			cancel()
			if err != nil {
				panic(err)
			}
			sendStatusUpdate = false
		}

		msg, err := f.replConn.ReceiveMessage(ctx)
		if err != nil && pgconn.Timeout(err) {
			// If the parent context was not canceled, something's wrong and
			// it's better to panic.  Otherwise we shut down cleanly.  It's not
			// a biggie if the replication connection was left in a bad state,
			// since the tester will restart it on test failures.
			if ctx.Err() == nil {
				panic(err)
			}

			f.replMessageChan <- nil
			close(f.replMessageChan)

			return
		} else if err != nil {
			panic(err)
		}

		var copyData *pgproto3.CopyData
		switch msg := msg.(type) {
			case *pgproto3.CopyData:
				copyData = msg
			case *pgproto3.ErrorResponse:
				panic(fmt.Sprintf("%#+v", msg))
			case *pgproto3.ParameterStatus:
				// ignore
				continue
			default:
				panic(fmt.Sprintf("%#+v", msg))
		}

		if copyData.Data[0] == pglogrepl.PrimaryKeepaliveMessageByteID {
			pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(copyData.Data[1:])
			if err != nil {
				panic(err)
			}
			sendStatusUpdate = pkm.ReplyRequested
		} else if copyData.Data[0] == pglogrepl.XLogDataByteID {
			xld, err := pglogrepl.ParseXLogData(copyData.Data[1:])
			if err != nil {
				panic(err)
			}
			decodedMessages, err := f.parseWireMessage(xld.WALData)
			if err != nil {
				f.replMessageChan <- &DecodedMessage{
					LSN: xld.WALStart,
					Err: err,
				}
			} else {
				for _, msg := range decodedMessages {
					f.replMessageChan <- &DecodedMessage{
						LSN: xld.WALStart,
						Message: msg,
					}
				}
			}

			clientLSN = xld.WALStart + pglogrepl.LSN(len(xld.WALData)) + 1
		}
	}
}

func (f *Fuzzer) parseWireMessage(data []byte) ([]proto.Message, error) {
	if len(data) < 3 {
		return nil, fmt.Errorf("unexpected wire message %+#v length %d", data, len(data))
	}
	header_len := int32(0)
	for i := 0; ; i++ {
		if i > 6 || i >= len(data) {
			return nil, fmt.Errorf("could not parse wire message header %+#v", data)
		}
		header_len = int32(data[i] & 0x7F);
		if (data[i] & 0x7F) == data[i] {
			data = data[i + 1:]
			break
		}
	}

	wireMsg := &WireMessageHeader{}
	err := proto.Unmarshal(data[:header_len], wireMsg)
	if err != nil {
		return nil, fmt.Errorf("could not unmarshal WireMessageHeader: %s", err)
	}
	data = data[header_len:]

	if len(wireMsg.Types) != len(wireMsg.Offsets) {
		return nil, fmt.Errorf(
			"invalid wireMsg: len(Types) %d != len(Offsets) %d",
			len(wireMsg.Types),
			len(wireMsg.Offsets),
		)
	}

	messages := make([]proto.Message, len(wireMsg.Types))
	for i, typ := range wireMsg.Types {
		offset := wireMsg.Offsets[i]
		if offset > int32(len(data)) {
			return nil, fmt.Errorf(
				"invalid wireMsg: offset %d > len(data) %d",
				offset,
				len(data),
			)
		}
		msgData := data[offset:]
		if i + 1 < len(wireMsg.Offsets) {
			nextOffset := wireMsg.Offsets[i + 1]
			msgLen := nextOffset - offset
			msgData = msgData[:msgLen]
		}

		switch typ {
			case WireMessageType_WMSG_BEGIN:
				begin := &BeginTransaction{}
				err = proto.Unmarshal(msgData, begin)
				if err != nil {
					return nil, fmt.Errorf("could not unmarshal BeginTransaction: %s", err)
				}
				messages[i] = begin
			case WireMessageType_WMSG_COMMIT:
				commit := &CommitTransaction{}
				err = proto.Unmarshal(msgData, commit)
				if err != nil {
					return nil, fmt.Errorf("could not unmarshal CommitTransaction: %s", err)
				}
				messages[i] = commit
			case WireMessageType_WMSG_INSERT:
				ins := &InsertDescription{}
				err = proto.Unmarshal(msgData, ins)
				if err != nil {
					return nil, fmt.Errorf("could not unmarshal InsertDescription: %s", err)
				}
				messages[i] = ins
			case WireMessageType_WMSG_UPDATE:
				upd := &UpdateDescription{}
				err = proto.Unmarshal(msgData, upd)
				if err != nil {
					return nil, fmt.Errorf("could not unmarshal UpdateDescription: %s", err)
				}
				messages[i] = upd
			case WireMessageType_WMSG_DELETE:
				del := &DeleteDescription{}
				err = proto.Unmarshal(msgData, del)
				if err != nil {
					return nil, fmt.Errorf("could not unmarshal DeleteDescription: %s", err)
				}
				messages[i] = del
			default:
				return nil, fmt.Errorf("unknown wire message type %+#v", typ)
		}
	}

	return messages, nil
}

func (f *Fuzzer) logFuzzError(prefix string, fuzzErr error, datas ...string) {
	datas = append(datas, fuzzErr.Error())
	errContext, ok := fuzzErr.(*FuzzerError)
	if ok {
		if errContext.Transaction != nil {
			datas = append(datas, fmt.Sprintf("\nTRANSACTION:\n\n%v\n", errContext.Transaction.Describe()))
		}
		if errContext.ExpectedMessages != nil {
			datas = append(datas, fmt.Sprintf("\nEXPECTED MESSAGES:\n\n%+v\n", errContext.DescribeExpectedMessages()))
		}
		if errContext.ReceivedMessages != nil {
			datas = append(datas, fmt.Sprintf("\nRECEIVED MESSAGES:\n\n%+v\n", errContext.DescribeReceivedMessages()))
		}
	}
	data := []byte(strings.Join(datas, "\n\n------\n\n") + "\n")
	filename := prefix + time.Now().Format("20060102150405.999") + ".log"
	err := os.WriteFile(filepath.Join("errors", filename), data, 0644)
	if err != nil {
		panic(err)
	}
	log.Printf("%s failure: %s", prefix, fuzzErr)
}

func main() {
	var seed int64
	err := binary.Read(crand.Reader, binary.BigEndian, &seed)
	if err != nil {
		panic(err)
	}
	rand.Seed(seed)

	conninfo := []string{
		"sslmode=disable",
		// required for predictability
		"synchronous_commit=on",
	}
	fuzzer := NewFuzzer(conninfo)
	fuzzer.MainLoop()
}

func testSetup(dbh *pgx.Conn, sql string) error {
	_, err := dbh.Exec(context.Background(), sql)
	if err != nil {
		return err
	}
	return nil
}
