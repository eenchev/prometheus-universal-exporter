//go:build request_type_modbus

package fetch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// modbusDevice is a Modbus TCP device as the tests need one: holding and
// input registers and coils by address, answering the four functions that
// read, with exception 2 for an address it has not and exception 11 for a
// unit that is not its own; and it keeps every request it was sent.
type modbusDevice struct {
	*testutil.TCPStub
	mu       sync.Mutex
	unit     byte
	holding  map[uint16]uint16
	input    map[uint16]uint16
	coils    map[uint16]bool
	requests [][]byte
	// mangle changes an answer before it is sent.
	mangle func([]byte) []byte
}

// newModbusDevice starts the device, as configure filled it before it took
// a connection.
func newModbusDevice(t *testing.T, configure ...func(*modbusDevice)) *modbusDevice {
	t.Helper()
	d := &modbusDevice{unit: 1, holding: map[uint16]uint16{}, input: map[uint16]uint16{}, coils: map[uint16]bool{}}
	for _, fn := range configure {
		fn(d)
	}
	d.TCPStub = testutil.NewTCPStub(t, d.serve)
	return d
}

// asked is the requests the device was sent.
func (d *modbusDevice) asked() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]byte(nil), d.requests...)
}

func (d *modbusDevice) serve(conn net.Conn) {
	for {
		request := make([]byte, 12)
		if _, err := io.ReadFull(conn, request); err != nil {
			return
		}
		d.mu.Lock()
		d.requests = append(d.requests, request)
		function, address, quantity := request[7], binary.BigEndian.Uint16(request[8:]), int(binary.BigEndian.Uint16(request[10:]))
		var pdu []byte
		exception := func(code byte) { pdu = []byte{function | 0x80, code} }
		switch {
		case request[6] != d.unit:
			exception(11)
		case function == 3 || function == 4:
			table := map[byte]map[uint16]uint16{3: d.holding, 4: d.input}[function]
			pdu = []byte{function, byte(quantity * 2)}
			for i := range quantity {
				value, ok := table[address+uint16(i)]
				if !ok {
					exception(2)
					break
				}
				pdu = binary.BigEndian.AppendUint16(pdu, value)
			}
		case function == 1 || function == 2:
			bits := make([]byte, (quantity+7)/8)
			for i := range quantity {
				if d.coils[address+uint16(i)] {
					bits[i/8] |= 1 << (i % 8)
				}
			}
			pdu = append([]byte{function, byte(len(bits))}, bits...)
		default:
			exception(1)
		}
		answer := append([]byte{request[0], request[1], 0, 0, 0, byte(len(pdu) + 1), request[6]}, pdu...)
		if d.mangle != nil {
			answer = d.mangle(answer)
		}
		d.mu.Unlock()
		if _, err := conn.Write(answer); err != nil {
			return
		}
	}
}

// set puts the registers of values at address, the high one first.
func (d *modbusDevice) set(table map[uint16]uint16, address uint16, bytes ...byte) {
	for i := 0; i < len(bytes); i += 2 {
		table[address+uint16(i/2)] = binary.BigEndian.Uint16(bytes[i:])
	}
}

func modbusCollector(t *testing.T, own ModbusConfig, change ...func(*model.RequestConfig)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "meter", Request: model.RequestConfig{Type: RequestTypeModbus, Own: own}}
	for _, fn := range change {
		fn(&c.Request)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func modbusDoc(t *testing.T, c *model.Collector, target string) map[string]any {
	t.Helper()
	resp, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(resp.Body)))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("%v: %s", err, resp.Body)
	}
	return doc
}

// A scrape reads each of the collector's reads with the function of its
// table and puts its value under its name: numbers of every type, in every
// order of bytes, a list where the read counts several, a string without
// its padding, and bits as true and false.
func TestAModbusScrapeReadsRegistersAndBits(t *testing.T) {
	d := newModbusDevice(t, func(d *modbusDevice) {
		d.holding[0] = 0xFFFE // -2 as i16
		float := make([]byte, 4)
		binary.BigEndian.PutUint32(float, math.Float32bits(230.5))
		d.set(d.input, 10, float...)                               // abcd
		d.set(d.input, 20, float[2], float[3], float[0], float[1]) // cdab
		d.set(d.input, 30, float[1], float[0], float[3], float[2]) // badc
		d.set(d.input, 40, float[3], float[2], float[1], float[0]) // dcba
		d.set(d.holding, 100, 0, 1, 0, 2, 0xFF, 0xFF, 0xFF, 0xFF)  // u32 65538, i32 -1
		d.set(d.holding, 200, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
		d.set(d.holding, 300, 'S', 'N', '-', '4', '2', 0, 'x', ' ')
		// A string whose device puts the low byte first: its registers
		// stay in their order, whatever the order says of a number's.
		d.set(d.holding, 310, 'B', 'A', 'D', 'C', 0, 'E')
		nan := make([]byte, 4)
		binary.BigEndian.PutUint32(nan, math.Float32bits(float32(math.NaN())))
		d.set(d.holding, 400, nan...)
		d.coils[5], d.coils[7], d.coils[14] = true, true, true
	})
	c := modbusCollector(t, ModbusConfig{Read: []ModbusRead{
		{Name: "word", Address: 0},
		{Name: "signed", Address: 0, Type: "i16"},
		{Name: "volts", Table: "input", Address: 10, Type: "f32"},
		{Name: "volts_cdab", Table: "input", Address: 20, Type: "f32", Order: "cdab"},
		{Name: "volts_badc", Table: "input", Address: 30, Type: "f32", Order: "badc"},
		{Name: "volts_dcba", Table: "input", Address: 40, Type: "f32", Order: "dcba"},
		{Name: "pair", Address: 100, Type: "i32", Count: 2},
		{Name: "counter", Address: 100, Type: "u32"},
		{Name: "big", Address: 200, Type: "i64"},
		{Name: "serial", Address: 300, Type: "string", Count: 4},
		{Name: "serial_swapped", Address: 310, Type: "string", Count: 3, Order: "dcba"},
		{Name: "missing", Address: 400, Type: "f32"},
		{Name: "bits", Table: "coil", Address: 5, Count: 10},
		{Name: "bit", Table: "discrete", Address: 7},
	}})
	doc := modbusDoc(t, c, d.Addr)
	want := map[string]string{
		"word": "65534", "signed": "-2", "volts": "230.5", "volts_cdab": "230.5", "volts_badc": "230.5", "volts_dcba": "230.5",
		"pair": "[65538,-1]", "counter": "65538", "big": "9223372036854775807", "serial": `"SN-42"`, "serial_swapped": `"ABCDE"`, "missing": "null",
		"bits": "[true,false,true,false,false,false,false,false,false,true]", "bit": "true",
	}
	for name, value := range want {
		got, _ := json.Marshal(doc[name])
		if string(got) != value {
			t.Errorf("%s is %s, want %s", name, got, value)
		}
	}
	if len(doc) != len(want) || d.Accepted() != 1 {
		t.Errorf("the document has %d values on %d connections, want %d on one", len(doc), d.Accepted(), len(want))
	}
	// Each read is one request: the function of its table, its address and
	// how many registers its values take.
	requests := d.asked()
	if first, pair := requests[0], requests[6]; first[7] != 3 || binary.BigEndian.Uint16(first[10:]) != 1 || pair[7] != 3 || binary.BigEndian.Uint16(pair[8:]) != 100 || binary.BigEndian.Uint16(pair[10:]) != 4 {
		t.Errorf("the requests were % x and % x", first, pair)
	}
	if bits := requests[12]; bits[7] != 1 || binary.BigEndian.Uint16(bits[10:]) != 10 {
		t.Errorf("the coils were asked with % x", bits)
	}
}

// The unit asked is the collector's, 1 when it names none, and the
// target's own when it is written modbus://host:port/<unit>.
func TestAModbusTargetNamesItsUnit(t *testing.T) {
	d := newModbusDevice(t, func(d *modbusDevice) { d.unit, d.holding[0] = 17, 7 })
	read := []ModbusRead{{Name: "value", Address: 0}}
	unit := 17
	for name, tc := range map[string]struct {
		own    ModbusConfig
		target string
	}{
		"the collector's": {ModbusConfig{Unit: &unit, Read: read}, d.Addr},
		"the target's":    {ModbusConfig{Read: read}, "modbus://" + d.Addr + "/17"},
	} {
		if doc := modbusDoc(t, modbusCollector(t, tc.own), tc.target); doc["value"].(json.Number) != "7" {
			t.Errorf("%s: %v", name, doc)
		}
	}
	c := modbusCollector(t, ModbusConfig{Read: read})
	if label, err := RequestLabelFor(d.Addr, c, RequestOverrides{}); err != nil || label != "modbus://"+d.Addr+"/1" {
		t.Errorf("the label is %q, %v", label, err)
	}
	// Unit 1 is not this device's: a gateway says no device answers.
	_, err := FetchCollector(context.Background(), d.Addr, c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "request.read[0] (value) of modbus://"+d.Addr+"/1: the device answered with exception 11, gateway target device failed to respond") {
		t.Errorf("err=%v, want the exception named", err)
	}
	for _, target := range []string{"modbus://" + d.Addr + "/256", "modbus://" + d.Addr + "/a", "tcp://" + d.Addr, "modbus://" + d.Addr + "/1/2"} {
		if _, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil); err == nil {
			t.Errorf("the target %q is taken", target)
		}
	}
}

// An exception fails the scrape, naming the read; so does an answer that
// is not the request's: another transaction, another function, or fewer
// bytes than the registers asked take.
func TestAModbusScrapeFailsOnWhatIsNoAnswer(t *testing.T) {
	c := modbusCollector(t, ModbusConfig{Read: []ModbusRead{{Name: "value", Address: 9, Count: 2}}})
	for name, tc := range map[string]struct {
		mangle func([]byte) []byte
		want   string
	}{
		"exception":   {nil, "exception 2, illegal data address"},
		"transaction": {func(b []byte) []byte { b[1]++; return b }, "the answer is of transaction 2, not 1"},
		"function":    {func(b []byte) []byte { b[7] = 4; return b }, "the answer is of function 4, not 3"},
		"short":       {func([]byte) []byte { return []byte{0, 1, 0, 0, 0, 5, 1, 3, 2, 0, 7} }, "the answer holds 2 bytes of data and says 2, where 2 registers take 4"},
		"no modbus":   {func([]byte) []byte { return []byte("HTTP/1.1 400 Bad Request\r\n\r\n") }, "which is no Modbus TCP header"},
	} {
		d := newModbusDevice(t, func(d *modbusDevice) {
			if tc.mangle != nil {
				d.holding[9], d.holding[10] = 1, 2
			}
			d.mangle = tc.mangle
		})
		_, err := FetchCollector(context.Background(), d.Addr, c, RequestOverrides{}, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "request.read[0] (value)") {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
		// What the answer held is not told to whoever called the probe,
		// but for an exception the protocol defines.
		if told := model.CallerText(err); name != "exception" && (strings.Contains(told, "48 54 54 50") || !strings.Contains(told, model.WithheldMark)) || name == "exception" && told != err.Error() {
			t.Errorf("%s: a probe's caller is told %q", name, told)
		}
	}
}

// A collector is refused at load for a read that is none.
func TestModbusConfigurationRules(t *testing.T) {
	unit := 300
	for name, tc := range map[string]struct {
		own  ModbusConfig
		want string
	}{
		"no read":      {ModbusConfig{}, "request.read is required"},
		"unit":         {ModbusConfig{Unit: &unit, Read: []ModbusRead{{Name: "a"}}}, "request.unit 300 is not a unit id"},
		"name":         {ModbusConfig{Read: []ModbusRead{{Name: "1a"}}}, `request.read[0].name "1a" is not a name`},
		"twice":        {ModbusConfig{Read: []ModbusRead{{Name: "a"}, {Name: "a", Address: 1}}}, `request.read[1].name "a" is another read's too`},
		"table":        {ModbusConfig{Read: []ModbusRead{{Name: "a", Table: "holdings"}}}, `request.read[0].table "holdings" is not holding, input, coil or discrete`},
		"type":         {ModbusConfig{Read: []ModbusRead{{Name: "a", Type: "u8"}}}, `request.read[0].type "u8" is not u16`},
		"bit type":     {ModbusConfig{Read: []ModbusRead{{Name: "a", Table: "coil", Type: "u16"}}}, "is not for the coil table"},
		"order":        {ModbusConfig{Read: []ModbusRead{{Name: "a", Order: "little"}}}, `request.read[0].order "little" is not abcd`},
		"too many":     {ModbusConfig{Read: []ModbusRead{{Name: "a", Type: "f64", Count: 32}}}, "reads 128 registers, more than the 125"},
		"too many bit": {ModbusConfig{Read: []ModbusRead{{Name: "a", Table: "coil", Count: 2001}}}, "reads 2001 bits, more than the 2000"},
		"address":      {ModbusConfig{Read: []ModbusRead{{Name: "a", Address: 40001}, {Name: "b", Address: 65535, Type: "u32"}}}, "request.read[1].address 65535, with 2 registers read from it, is outside 0 to 65535"},
	} {
		c := &model.Collector{Name: "meter", Request: model.RequestConfig{Type: RequestTypeModbus, Own: tc.own}}
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
}

// A device among the collector's denied targets is refused before it is
// connected to.
func TestAModbusCollectorHoldsTheTargetPolicy(t *testing.T) {
	d := newModbusDevice(t)
	c := modbusCollector(t, ModbusConfig{Read: []ModbusRead{{Name: "a"}}}, func(r *model.RequestConfig) { r.DeniedTargets = []string{"127.0.0.0/8"} })
	if _, err := FetchCollector(context.Background(), d.Addr, c, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) || d.Accepted() != 0 {
		t.Errorf("err=%v, %d connections; want the target refused", err, d.Accepted())
	}
}

// The keys the opt-in modbus type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheModbusTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeModbus)
}
