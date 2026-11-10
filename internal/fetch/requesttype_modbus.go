//go:build request_type_modbus

package fetch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The modbus request type reads registers and bits from a device that
// speaks Modbus TCP: a PLC, an inverter, a meter, or a gateway with serial
// devices behind it.
//
//	request:
//	  type: modbus
//	  unit: 1                      # the unit id; 1 when left out
//	  read:
//	    - name: voltage
//	      table: input             # holding (default), input, coil or discrete
//	      address: 0               # as the protocol numbers it, from 0
//	      type: f32                # u16 (default), i16, u32, i32, u64, i64, f32, f64, string
//	      count: 3                 # how many values; 1 when left out
//	      order: cdab              # abcd (default), badc, cdab or dcba
//
// Each read is one request of the function its table has (1 coils, 2
// discrete inputs, 3 holding registers, 4 input registers), sent one after
// another on one connection the scrape opens and closes. The answer handed
// to the transforms is one JSON document, each read's value under its
// name: a number, true or false for a bit, text for a string, and a list of
// them when count is more than 1.
//
// The type only reads: no function that writes is in the code. A device's
// exception fails the scrape, naming the read and the exception.
//
// The target is host, host:port or modbus://host:port, port 502, and
// modbus://host:port/<unit> for a unit other than the collector's, as the
// devices behind one gateway are told apart.
//
// The type imports nothing outside the standard library.

// ModbusConfig is a modbus collector's own request keys.
type ModbusConfig struct {
	Unit *int         `yaml:"unit,omitempty"`
	Read []ModbusRead `yaml:"read"`
}

// ModbusRead is one read of a modbus collector.
type ModbusRead struct {
	Name    string `yaml:"name"`
	Table   string `yaml:"table,omitempty"`
	Address int    `yaml:"address"`
	Type    string `yaml:"type,omitempty"`
	Count   int    `yaml:"count,omitempty"`
	Order   string `yaml:"order,omitempty"`
}

func modbusRequest(c *model.Collector) ModbusConfig {
	own, _ := c.Request.Own.(ModbusConfig)
	return own
}

const (
	// modbusTimeout bounds a scrape with no deadline of its own.
	modbusTimeout = 10 * time.Second
	// modbusMaxReads bounds request.read.
	modbusMaxReads = 64
	// The most one request may ask for (Modbus Application Protocol 1.1b3,
	// 6.1 to 6.4).
	modbusMaxRegisters = 125
	modbusMaxBits      = 2000
	modbusDefaultUnit  = 1
)

// modbusPorts are the schemes of a modbus target and their default ports.
var modbusPorts = map[string]int{"": 502, "modbus": 502}

// modbusTables are the tables and the function that reads each, and
// whether the table is of bits.
var modbusTables = map[string]struct {
	function byte
	bits     bool
}{
	"coil":     {1, true},
	"discrete": {2, true},
	"holding":  {3, false},
	"input":    {4, false},
}

// modbusWidths are the types of a register read and how many registers a
// value of each takes; a string's count is its registers.
var modbusWidths = map[string]int{"u16": 1, "i16": 1, "u32": 2, "i32": 2, "f32": 2, "u64": 4, "i64": 4, "f64": 4, "string": 1}

// modbusExceptions are the exception codes a device answers with.
var modbusExceptions = map[byte]string{
	1:  "illegal function: the device does not have this table, or does not let it be read",
	2:  "illegal data address: the device has no such address, or the read runs past its last one",
	3:  "illegal data value: the device does not take a read of this many",
	4:  "server device failure",
	5:  "acknowledge: the device is still working on the request",
	6:  "server device busy",
	8:  "memory parity error",
	10: "gateway path unavailable: the gateway has no path to this unit",
	11: "gateway target device failed to respond: no device answers as this unit",
}

var modbusName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeModbus,
		Fields:       []string{"allowed_targets", "denied_targets"},
		Config:       ModbusConfig{},
		KeyRules:     modbusKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    modbusRequester{},
		JSONAnswer:   true,
		Stage:        "modbus",
		TargetHint: func(*model.Collector) (string, string) {
			return "Device", "host, host:port or modbus://host:502/<unit>"
		},
		AnswerHint: "Paste the JSON document the exporter makes of the values read, as the Response of a run shows it",
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		SchemaRule: func() map[string]any {
			return map[string]any{"required": []any{"read"}}
		},
	})
}

// modbusRequester is the modbus request type's Requester.
type modbusRequester struct{}

// Validate checks a modbus collector's request and fills in its defaults.
func (modbusRequester) Validate(x *model.Collector) error {
	own := modbusRequest(x)
	if own.Unit != nil && (*own.Unit < 0 || *own.Unit > 255) {
		return fmt.Errorf("collector %q request.unit %d is not a unit id, 0 to 255", x.Name, *own.Unit)
	}
	if len(own.Read) == 0 && !x.RequestNotMade {
		return fmt.Errorf("collector %q request.read is required for request.type modbus: the registers or bits the scrape reads", x.Name)
	}
	if len(own.Read) > modbusMaxReads {
		return fmt.Errorf("collector %q request.read has %d reads, more than %d", x.Name, len(own.Read), modbusMaxReads)
	}
	names := map[string]bool{}
	reads := make([]ModbusRead, len(own.Read))
	for i, r := range own.Read {
		where := fmt.Sprintf("collector %q request.read[%d]", x.Name, i)
		if !modbusName.MatchString(r.Name) {
			return fmt.Errorf("%s.name %q is not a name: letters, digits and _, not starting with a digit", where, r.Name)
		}
		if names[r.Name] {
			return fmt.Errorf("%s.name %q is another read's too", where, r.Name)
		}
		names[r.Name] = true
		if r.Table == "" {
			r.Table = "holding"
		}
		table, ok := modbusTables[r.Table]
		if !ok {
			return fmt.Errorf("%s.table %q is not holding, input, coil or discrete", where, r.Table)
		}
		if r.Count == 0 {
			r.Count = 1
		}
		if r.Count < 0 {
			return fmt.Errorf("%s.count %d is negative", where, r.Count)
		}
		quantity, most, unit := r.Count, modbusMaxBits, "bits"
		if table.bits {
			if r.Type != "" && r.Type != "bool" {
				return fmt.Errorf("%s.type %q is not for the %s table, whose values are bits; leave type out", where, r.Type, r.Table)
			}
			if r.Order != "" {
				return fmt.Errorf("%s.order is not for the %s table, whose values are bits", where, r.Table)
			}
			r.Type = "bool"
		} else {
			if r.Type == "" {
				r.Type = "u16"
			}
			width, ok := modbusWidths[r.Type]
			if !ok {
				return fmt.Errorf("%s.type %q is not u16, i16, u32, i32, u64, i64, f32, f64 or string", where, r.Type)
			}
			switch r.Order {
			case "":
				r.Order = "abcd"
			case "abcd", "badc", "cdab", "dcba":
			default:
				return fmt.Errorf("%s.order %q is not abcd, badc, cdab or dcba", where, r.Order)
			}
			quantity, most, unit = r.Count*width, modbusMaxRegisters, "registers"
		}
		if quantity > most {
			return fmt.Errorf("%s reads %d %s, more than the %d one Modbus request may ask for; split it into several reads", where, quantity, unit, most)
		}
		if r.Address < 0 || r.Address+quantity > 65536 {
			return fmt.Errorf("%s.address %d, with %d %s read from it, is outside 0 to 65535; an address is as the protocol numbers it, from 0, so a register a manual calls 40001 is address 0 of the holding table", where, r.Address, quantity, unit)
		}
		reads[i] = r
	}
	own.Read = reads
	x.Request.Own = own
	return nil
}

// modbusTarget reads a target and the unit it names, the collector's
// when it names none.
func modbusTarget(c *model.Collector, target string) (netAddress, byte, error) {
	a, err := parseNetTarget("modbus", target, modbusPorts, true)
	if err != nil {
		return a, 0, err
	}
	unit := modbusDefaultUnit
	if own := modbusRequest(c); own.Unit != nil {
		unit = *own.Unit
	}
	if rest := strings.TrimPrefix(a.rest, "/"); rest != "" {
		n, err := strconv.Atoi(rest)
		if err != nil || n < 0 || n > 255 || strconv.Itoa(n) != rest {
			return a, 0, fmt.Errorf("target %q has %q after its port, which is not a unit id, 0 to 255, as modbus://host:502/17 has one", target, rest)
		}
		unit = n
	}
	return a, byte(unit), nil
}

// CheckTarget checks a modbus collector's target.
func (modbusRequester) CheckTarget(c *model.Collector, target string, _ bool) error {
	_, _, err := modbusTarget(c, target)
	return err
}

// Label is the url label of a modbus request: modbus://host:port/unit.
func (modbusRequester) Label(target string, c *model.Collector, _ RequestOverrides) (string, error) {
	a, unit, err := modbusTarget(c, target)
	if err != nil {
		return "", err
	}
	return modbusURL(a, unit), nil
}

func modbusURL(a netAddress, unit byte) string {
	return "modbus://" + a.hostPort + "/" + strconv.Itoa(int(unit))
}

// Method is the http_method label of a modbus request.
func (modbusRequester) Method(*model.Collector, RequestOverrides) string { return "READ" }

// Display renders a modbus target for logs and labels.
func (modbusRequester) Display(target string) string {
	return netDisplay("modbus", target, modbusPorts, true)
}

// Fetch performs the reads of a modbus collector on one connection.
func (modbusRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	address, unit, err := modbusTarget(c, target)
	if err != nil {
		return nil, err
	}
	ctx, cancel := netBudget(ctx, overrides, modbusTimeout)
	defer cancel()
	if ctx, err = netGuard(ctx, c, "modbus", address.hostPort); err != nil {
		return nil, err
	}
	label := modbusURL(address, unit)
	traceRequest(ctx, "CONNECT", label, nil, "", false)
	conn, err := netDial(ctx, address, nil)
	if err != nil {
		traceFailedOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	defer conn.close()
	traceOutcome(ctx, "connected")
	values := map[string]any{}
	for i, read := range modbusRequest(c).Read {
		shown := fmt.Sprintf("%s %s %d", read.Name, read.Table, read.Address)
		traceRequest(ctx, "READ", label, http.Header{"Modbus-Read": {shown}}, "", false)
		data, err := modbusExchange(ctx, conn, uint16(i+1), unit, read)
		if err != nil {
			err = fmt.Errorf("request.read[%d] (%s) of %s: %w", i, read.Name, label, err)
			traceFailedOutcome(ctx, "error: "+err.Error())
			return nil, err
		}
		traceOutcome(ctx, strconv.Itoa(len(data))+" bytes")
		values[read.Name] = modbusValues(read, data)
	}
	body, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	traceFacts(ctx, intFact("modbus.reads", len(values)), intFact("modbus.unit", int(unit)))
	return &HTTPResponse{StatusCode: http.StatusOK, NoStatus: true, Headers: http.Header{"Content-Type": {"application/json"}}, Body: body, Target: target, Collector: c.Name, Duration: time.Since(start)}, nil
}

// modbusQuantity is how many registers or bits a read asks for.
func modbusQuantity(read ModbusRead) int {
	if modbusTables[read.Table].bits {
		return read.Count
	}
	return read.Count * modbusWidths[read.Type]
}

// modbusExchange sends one read and returns the data bytes of its answer:
// the MBAP header (transaction, protocol 0, length, unit), then the
// function, the address and the quantity; the answer has the same header,
// the function, a count of bytes and the bytes.
func modbusExchange(ctx context.Context, conn *netConn, transaction uint16, unit byte, read ModbusRead) ([]byte, error) {
	table := modbusTables[read.Table]
	quantity := modbusQuantity(read)
	want := quantity * 2
	if table.bits {
		want = (quantity + 7) / 8
	}
	request := make([]byte, 12)
	binary.BigEndian.PutUint16(request[0:], transaction)
	binary.BigEndian.PutUint16(request[4:], 6)
	request[6], request[7] = unit, table.function
	binary.BigEndian.PutUint16(request[8:], uint16(read.Address)) // #nosec G115 -- Validate holds it to 0..65535
	binary.BigEndian.PutUint16(request[10:], uint16(quantity))    // #nosec G115 -- at most 2000
	if _, err := conn.Write(request); err != nil {
		return nil, netFailure(ctx, err)
	}
	header := make([]byte, 8)
	if _, err := io.ReadFull(conn.r, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = errors.New("the device closed the connection before it answered")
		}
		return nil, netFailure(ctx, err)
	}
	length := int(binary.BigEndian.Uint16(header[4:]))
	switch {
	case binary.BigEndian.Uint16(header[2:]) != 0 || length < 3 || length > 254:
		return nil, model.Errorf("the answer begins %s, which is no Modbus TCP header; is the target a Modbus TCP device?", model.Bare(fmt.Sprintf("% x", header)))
	case binary.BigEndian.Uint16(header[0:]) != transaction:
		return nil, model.Errorf("the answer is of transaction %s, not %d, the one asked", model.Bare(strconv.Itoa(int(binary.BigEndian.Uint16(header[0:])))), transaction)
	case header[6] != unit:
		return nil, model.Errorf("the answer is of unit %s, not %d, the one asked", model.Bare(strconv.Itoa(int(header[6]))), unit)
	}
	rest := make([]byte, length-2)
	if _, err := io.ReadFull(conn.r, rest); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = errors.New("the device closed the connection in the middle of its answer")
		}
		return nil, netFailure(ctx, err)
	}
	if header[7] == table.function|0x80 {
		// A code the protocol defines is told to whoever asked; another
		// is a value of the answer.
		if meaning, known := modbusExceptions[rest[0]]; known {
			return nil, fmt.Errorf("the device answered with exception %d, %s", rest[0], meaning)
		}
		return nil, model.Errorf("the device answered with exception %s, a code the protocol does not define", model.Bare(strconv.Itoa(int(rest[0]))))
	}
	if header[7] != table.function {
		return nil, model.Errorf("the answer is of function %s, not %d, the one asked", model.Bare(strconv.Itoa(int(header[7]))), table.function)
	}
	if int(rest[0]) != want || len(rest)-1 != want {
		return nil, model.Errorf("the answer holds %s bytes of data and says %s, where %d %s take %d", model.Bare(strconv.Itoa(len(rest)-1)), model.Bare(strconv.Itoa(int(rest[0]))), quantity, map[bool]string{true: "bits", false: "registers"}[table.bits], want)
	}
	return rest[1:], nil
}

// modbusValues reads a read's values from the data of its answer: one, or
// a list when the read counts more.
func modbusValues(read ModbusRead, data []byte) any {
	var values []any
	switch read.Type {
	case "bool":
		// The first bit asked is the lowest of the first byte.
		for i := range read.Count {
			values = append(values, data[i/8]>>(i%8)&1 == 1)
		}
	case "string":
		// Two characters a register, the high byte first unless the order
		// swaps them. A NUL ends the text, what follows it being whatever
		// the registers held before, and blanks at the end are padding.
		text, _, _ := strings.Cut(string(modbusOrdered(data, read.Order, 0)), "\x00")
		return strings.ToValidUTF8(strings.TrimRight(text, " "), "\uFFFD")
	default:
		width := modbusWidths[read.Type]
		for i := range read.Count {
			values = append(values, modbusNumber(read.Type, modbusOrdered(data[i*width*2:(i+1)*width*2], read.Order, width)))
		}
	}
	if read.Count == 1 {
		return values[0]
	}
	return values
}

// modbusOrdered is the bytes of a value of words registers in big-endian
// order: order names where the bytes of a 32-bit value ABCD are on the
// wire, and wider and narrower values follow the same two swaps, of the
// bytes in each register (badc, dcba) and of the registers (cdab, dcba).
// A string's registers are never reversed, only their bytes swapped:
// words is 0 for one.
func modbusOrdered(data []byte, order string, words int) []byte {
	out := make([]byte, len(data))
	swapBytes := order == "badc" || order == "dcba"
	swapWords := (order == "cdab" || order == "dcba") && words > 0
	for w := range len(data) / 2 {
		from := w
		if swapWords {
			from = len(data)/2 - 1 - w
		}
		hi, lo := data[from*2], data[from*2+1]
		if swapBytes {
			hi, lo = lo, hi
		}
		out[w*2], out[w*2+1] = hi, lo
	}
	return out
}

// modbusNumber is a value of kind read from its big-endian bytes; a float
// that is no number, which JSON cannot write, is null.
func modbusNumber(kind string, b []byte) any {
	switch kind {
	case "u16":
		return binary.BigEndian.Uint16(b)
	case "i16":
		return int16(binary.BigEndian.Uint16(b)) // #nosec G115 -- the register's bits as a signed number
	case "u32":
		return binary.BigEndian.Uint32(b)
	case "i32":
		return int32(binary.BigEndian.Uint32(b)) // #nosec G115 -- the registers' bits as a signed number
	case "u64":
		return binary.BigEndian.Uint64(b)
	case "i64":
		return int64(binary.BigEndian.Uint64(b)) // #nosec G115 -- the registers' bits as a signed number
	}
	value := float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
	if kind == "f64" {
		value = math.Float64frombits(binary.BigEndian.Uint64(b))
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return value
}

// modbusKeyRules is what the configuration schema says of a modbus
// collector's own keys.
func modbusKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"unit":           {"minimum": 0, "maximum": 255, "description": "modbus: the unit id asked, 0 to 255, which tells the devices behind a gateway apart; 1 when left out. A target written modbus://host:502/<unit> names its own. See docs/MODBUS.md."},
		"read":           {"minItems": 1, "maxItems": modbusMaxReads, "description": "modbus, required: the reads of a scrape, one request each, whose values the document holds under each read's name. See docs/MODBUS.md#reads."},
		"read[]":         {"required": []string{"name"}},
		"read[].name":    {"pattern": modbusName.String(), "description": "The name the value is put under in the document: letters, digits and _, not starting with a digit, unique among the collector's reads."},
		"read[].table":   {"enum": SchemaOptionalEnum([]string{"holding", "input", "coil", "discrete"}), "description": "The table read: holding, the default (function 3), input (4), coil (1) or discrete (2)."},
		"read[].address": {"minimum": 0, "maximum": 65535, "description": "The first address read, as the protocol numbers it, from 0: the register a manual calls 40001 is address 0 of the holding table, and 30001 address 0 of the input table."},
		"read[].type":    {"enum": SchemaOptionalEnum([]string{"u16", "i16", "u32", "i32", "u64", "i64", "f32", "f64", "string", "bool"}), "description": "What a value of a register table is: u16, the default, i16, u32, i32, u64, i64, f32, f64, or string, two characters a register. Not for coil and discrete, whose values are true or false."},
		"read[].count":   {"minimum": 0, "description": "How many values are read, 1 when left out; more than 1 makes the value a list. Of a string, how many registers it takes. At most 125 registers or 2000 bits a read."},
		"read[].order":   {"enum": SchemaOptionalEnum([]string{"abcd", "badc", "cdab", "dcba"}), "description": "Where the bytes of a value are in its registers: abcd, the default, big-endian; cdab, the registers swapped; badc, the bytes of each register swapped; dcba, both, little-endian."},
	}
}
