package decode

import (
	"encoding/json"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

// The influx decoder reads a point a line: the measurement, the tags as
// text, the fields by their types, a float a number, an integer and an
// unsigned integer whole numbers that keep every digit, a boolean in each
// of its spellings and text in quotes, and the timestamp as it is written,
// null when the line has none. Blank lines and comments are passed over,
// and an escaped comma, equals sign and blank are those characters.
func TestTheInfluxDecoderReadsAPointALine(t *testing.T) {
	body := "# Telegraf\n" +
		"cpu,host=web01,region=eu\\ west usage_idle=97.5,usage_user=1.5e0,cores=8i 1556813561098000000\r\n" +
		"\n" +
		"  disk\\ space,path=/var/lib\\,data free=12u,ok=t,note=\"a \\\"quoted\\\" \\\\ value, with=signs\" -5\n" +
		"up value=T,down=FALSE,name=\"\"\n" +
		"big\\,name,k\\=ey=v\\=alue f\\ ield=18446744073709551615u\t\n" +
		"last v=-0.5"
	got, err := decodeInflux([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"measurement": "cpu", "tags": map[string]any{"host": "web01", "region": "eu west"}, "fields": map[string]any{"usage_idle": 97.5, "usage_user": 1.5, "cores": 8}, "timestamp": 1556813561098000000},
		map[string]any{"measurement": "disk space", "tags": map[string]any{"path": "/var/lib,data"}, "fields": map[string]any{"free": 12, "ok": true, "note": `a "quoted" \ value, with=signs`}, "timestamp": -5},
		map[string]any{"measurement": "up", "tags": map[string]any{}, "fields": map[string]any{"value": true, "down": false, "name": ""}, "timestamp": nil},
		map[string]any{"measurement": "big,name", "tags": map[string]any{"k=ey": "v=alue"}, "fields": map[string]any{"f ield": new(big.Int).SetUint64(18446744073709551615)}, "timestamp": nil},
		map[string]any{"measurement": "last", "tags": map[string]any{}, "fields": map[string]any{"v": -0.5}, "timestamp": nil},
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		t.Errorf("decoded\n%s\nwant\n%s", gotJSON, wantJSON)
	}
	// A body of no point is an empty list, and a key written twice keeps
	// the later value.
	if got, err := decodeInflux([]byte("\n# nothing\n \n")); err != nil || len(got) != 0 || got == nil {
		t.Errorf("no point: %v, %v", got, err)
	}
	got, err = decodeInflux([]byte("m,a=1,a=2 f=1,f=2i"))
	if err != nil || !reflect.DeepEqual(got, []any{map[string]any{"measurement": "m", "tags": map[string]any{"a": "2"}, "fields": map[string]any{"f": 2}, "timestamp": nil}}) {
		t.Errorf("a key twice: %v, %v", got, err)
	}
	// A line feed in quoted text is part of the text, and the lines after
	// it are counted from it.
	got, err = decodeInflux([]byte("log msg=\"two\nlines\" 1\nbad"))
	if err == nil || !strings.Contains(err.Error(), "line 3: the line ends after the measurement and its tags") {
		t.Errorf("a line feed in text: %v, %v", got, err)
	}
}

// What is no line protocol is refused by its line and column, saying what
// was expected there and never more of the body.
func TestTheInfluxDecoderSaysWhereALineIsWrong(t *testing.T) {
	for body, want := range map[string]string{
		"cpu":                          "line 1: the line ends after the measurement and its tags; a blank and the fields follow them",
		"ok v=1\n,host=a v=1":          `line 2, column 1: unexpected ',' where a measurement begins the line`,
		"cpu,host v=1":                 `line 1, column 9: unexpected ' ' where a tag is written key=value`,
		"cpu,host= v=1":                `line 1, column 10: unexpected ' ' where the value of tag "host" is`,
		"cpu v":                        "line 1: the line ends where a field is written key=value",
		"cpu v=":                       `line 1: the line ends where the value of field "v" is`,
		"cpu v=1,":                     "line 1: the line ends where a field is written key=value",
		"cpu v=abc":                    `line 1, column 7: the value of field "v" is no value of a field; want a number, a whole number and i or u, a boolean or text in double quotes`,
		"cpu v=NaN":                    `the value of field "v" is no value of a field`,
		"cpu v=1.5i":                   `the value of field "v" is no 64-bit integer`,
		"cpu v=-1u":                    `the value of field "v" is no unsigned 64-bit integer`,
		"cpu v=1e999":                  `the value of field "v" is no number a float holds`,
		"cpu v=\"open":                 `line 1, column 7: the text of field "v" is not closed by a quote`,
		"cpu v=\"a\"b":                 `line 1, column 10: unexpected 'b' after the value of a field`,
		"cpu v=1 soon":                 `line 1, column 9: unexpected 's' where the timestamp is, a whole number`,
		"cpu v=1 12 34":                `line 1, column 12: unexpected '3' after the timestamp, which ends the line`,
		"cpu v=1 99999999999999999999": "line 1, column 9: the timestamp is beyond a 64-bit whole number",
	} {
		if _, err := decodeInflux([]byte(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err=%v, want %q", body, err, want)
		}
	}
}
